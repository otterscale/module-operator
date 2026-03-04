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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

const fieldManager = "addons-operator"

// serverSideApply applies a list of unstructured objects using
// server-side apply, ensuring the addons-operator owns the fields it manages.
func serverSideApply(ctx context.Context, restCfg *rest.Config, mapper meta.RESTMapper, objects []*unstructured.Unstructured, force bool) error {
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	for _, obj := range objects {
		gvk := obj.GroupVersionKind()
		mapping, mapErr := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if mapErr != nil {
			return fmt.Errorf("mapping GVK %s: %w", gvk, mapErr)
		}

		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			dr = dc.Resource(mapping.Resource).Namespace(ns)
		} else {
			dr = dc.Resource(mapping.Resource)
		}

		obj.SetManagedFields(nil)
		obj.SetResourceVersion("")

		data, marshalErr := obj.MarshalJSON()
		if marshalErr != nil {
			return fmt.Errorf("marshalling %s/%s: %w", gvk.Kind, obj.GetName(), marshalErr)
		}

		forceApply := force
		_, err := dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
			FieldManager: fieldManager,
			Force:        &forceApply,
		})
		if err != nil {
			return fmt.Errorf("applying %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}

	return nil
}

// pruneResources deletes resources that are present in the stale inventory
// entries but no longer part of the desired state.
func pruneResources(ctx context.Context, restCfg *rest.Config, mapper meta.RESTMapper, stale []addonsv1alpha1.InventoryEntry) error {
	if len(stale) == 0 {
		return nil
	}

	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client for prune: %w", err)
	}

	logger := log.FromContext(ctx)

	for _, entry := range stale {
		gvk := inventoryGVK(entry)
		ns, name, _, _ := parseInventoryID(entry.ID)

		mapping, mapErr := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if mapErr != nil {
			logger.V(1).Info("Skipping prune for unknown GVK", "gvk", gvk, "error", mapErr)
			continue
		}

		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			dr = dc.Resource(mapping.Resource).Namespace(ns)
		} else {
			dr = dc.Resource(mapping.Resource)
		}

		propagation := metav1.DeletePropagationBackground
		err := dr.Delete(ctx, name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("pruning %s %s/%s: %w", gvk.Kind, ns, name, err)
		}
		logger.Info("Pruned resource", "kind", gvk.Kind, "namespace", ns, "name", name)
	}

	return nil
}
