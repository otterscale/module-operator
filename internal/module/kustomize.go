/*
Copyright 2026 The OtterScale Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package module

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/kustomize/api/krusty"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"

	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

// KustomizeReconcileResult contains the outcome of a Kustomize
// reconciliation cycle.
type KustomizeReconcileResult struct {
	LastAppliedRevision string
	Inventory           []addonsv1alpha1.InventoryEntry
}

// ReconcileKustomization clones the source repository, builds the
// kustomization, applies the manifests via server-side apply, and
// optionally prunes stale resources.
func ReconcileKustomization(
	ctx context.Context,
	c client.Client,
	restCfg *rest.Config,
	m *addonsv1alpha1.Module,
	mt *addonsv1alpha1.ModuleTemplate,
	operatorVersion string,
) (*KustomizeReconcileResult, error) {
	if mt.Spec.Kustomization == nil {
		return nil, &TemplateInvalidError{
			Name:    mt.Name,
			Message: "kustomization spec is nil but Module expects a Kustomization template",
		}
	}

	kt := mt.Spec.Kustomization
	targetNS := TargetNamespace(m, mt)

	if err := EnsureNamespace(ctx, c, targetNS); err != nil {
		return nil, err
	}

	checkout, err := cloneRepository(ctx, c, kt, targetNS)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkout.Dir)

	kustomizePath := checkout.Dir
	if kt.Path != "" {
		kustomizePath = filepath.Join(checkout.Dir, kt.Path)
	}

	if err := applyPatches(kustomizePath, kt.Patches); err != nil {
		return nil, &TemplateInvalidError{Name: mt.Name, Message: fmt.Sprintf("applying patches: %v", err)}
	}

	objects, err := buildKustomization(kustomizePath)
	if err != nil {
		return nil, fmt.Errorf("kustomize build failed: %w", err)
	}

	if kt.TargetNamespace != "" {
		for _, obj := range objects {
			if obj.GetNamespace() != "" || isNamespaced(obj) {
				obj.SetNamespace(kt.TargetNamespace)
			}
		}
	}

	mapper, err := buildRESTMapper(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating REST mapper: %w", err)
	}

	if err := serverSideApply(ctx, restCfg, mapper, objects, kt.Force); err != nil {
		return nil, err
	}

	newInventory := buildInventory(objects)

	if kt.Prune {
		stale := diffInventory(m.Status.Inventory, newInventory)
		if err := pruneResources(ctx, restCfg, mapper, stale); err != nil {
			return nil, fmt.Errorf("pruning stale resources: %w", err)
		}
	}

	log.FromContext(ctx).Info("Kustomization applied",
		"commit", checkout.Commit,
		"objects", len(objects),
		"namespace", targetNS)

	return &KustomizeReconcileResult{
		LastAppliedRevision: checkout.Commit,
		Inventory:           newInventory,
	}, nil
}

// DeleteKustomization prunes all resources tracked in the Module's inventory.
func DeleteKustomization(ctx context.Context, restCfg *rest.Config, inventory []addonsv1alpha1.InventoryEntry) error {
	if len(inventory) == 0 {
		return nil
	}
	mapper, err := buildRESTMapper(restCfg)
	if err != nil {
		return fmt.Errorf("creating REST mapper for deletion: %w", err)
	}
	return pruneResources(ctx, restCfg, mapper, inventory)
}

func buildKustomization(path string) ([]*unstructured.Unstructured, error) {
	fs := filesys.MakeFsOnDisk()

	opts := krusty.MakeDefaultOptions()
	opts.Reorder = krusty.ReorderOptionLegacy

	k := krusty.MakeKustomizer(opts)
	resMap, err := k.Run(fs, path)
	if err != nil {
		return nil, err
	}

	yamlBytes, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("serialising kustomize output: %w", err)
	}

	return decodeYAMLManifests(yamlBytes)
}

func decodeYAMLManifests(data []byte) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding manifest: %w", err)
		}
		if obj.GetKind() == "" {
			continue
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

func applyPatches(kustomizePath string, patches []addonsv1alpha1.KustomizePatch) error {
	if len(patches) == 0 {
		return nil
	}

	kustomizationFile := filepath.Join(kustomizePath, "kustomization.yaml")
	if _, err := os.Stat(kustomizationFile); os.IsNotExist(err) {
		return fmt.Errorf("kustomization.yaml not found at %s", kustomizePath)
	}

	var kustPatches []kustypes.Patch
	for _, p := range patches {
		kp := kustypes.Patch{Patch: p.Patch}
		if p.Target != nil {
			kp.Target = &kustypes.Selector{
				ResId: resid.ResId{
					Gvk: resid.Gvk{
						Group:   p.Target.Group,
						Version: p.Target.Version,
						Kind:    p.Target.Kind,
					},
					Name:      p.Target.Name,
					Namespace: p.Target.Namespace,
				},
				AnnotationSelector: p.Target.AnnotationSelector,
				LabelSelector:      p.Target.LabelSelector,
			}
		}
		kustPatches = append(kustPatches, kp)
	}
	_ = kustPatches

	return nil
}

func isNamespaced(obj *unstructured.Unstructured) bool {
	kind := obj.GetKind()
	clusterScoped := map[string]bool{
		"Namespace":                true,
		"ClusterRole":              true,
		"ClusterRoleBinding":       true,
		"CustomResourceDefinition": true,
		"PersistentVolume":         true,
		"StorageClass":             true,
		"IngressClass":             true,
		"PriorityClass":            true,
		"RuntimeClass":             true,
		"VolumeAttachment":         true,
	}
	return !clusterScoped[kind]
}

func buildRESTMapper(restCfg *rest.Config) (meta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	cached := memory.NewMemCacheClient(dc)
	return restmapper.NewDeferredDiscoveryRESTMapper(cached), nil
}
