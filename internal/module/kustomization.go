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
	"encoding/json"
	"fmt"
	"maps"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

// ReconcileKustomization ensures the FluxCD Kustomization exists and matches the
// desired state derived from the ModuleTemplate.
//
// The Kustomization is created in the target namespace with OwnerReference
// pointing to the cluster-scoped Module for garbage collection.
func ReconcileKustomization(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	m *modulev1alpha1.Module,
	mt *modulev1alpha1.ModuleTemplate,
	version string,
) error {
	if mt.Spec.Kustomization == nil {
		return &TemplateInvalidError{
			Name:    mt.Name,
			Message: "kustomization spec is nil but Module expects a Kustomization template",
		}
	}

	var templateSpec kustomizev1.KustomizationSpec
	if err := json.Unmarshal(mt.Spec.Kustomization.Raw, &templateSpec); err != nil {
		return &TemplateInvalidError{
			Name:    mt.Name,
			Message: fmt.Sprintf("failed to unmarshal kustomization spec: %v", err),
		}
	}

	targetNS := TargetNamespace(m, mt)

	if err := EnsureNamespace(ctx, c, targetNS); err != nil {
		return err
	}

	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: targetNS,
		},
	}

	op, err := ctrlutil.CreateOrPatch(ctx, c, ks, func() error {
		ks.Spec = templateSpec

		// Ensure labels are set for identification and filtering
		if ks.Labels == nil {
			ks.Labels = map[string]string{}
		}
		maps.Copy(ks.Labels, LabelsForModule(m.Name, mt.Name, version))

		// Set OwnerReference for garbage collection.
		// Cluster-scoped owners CAN own namespace-scoped resources in Kubernetes.
		return ctrlutil.SetControllerReference(m, ks, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("Kustomization reconciled",
			"operation", op, "name", ks.Name, "namespace", ks.Namespace)
	}
	return nil
}

// DeleteKustomization deletes the FluxCD Kustomization associated with the Module.
// It returns nil if the resource is already gone.
func DeleteKustomization(ctx context.Context, c client.Client, m *modulev1alpha1.Module, namespace string) error {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: namespace,
		},
	}
	if err := c.Delete(ctx, ks); err != nil {
		return client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).Info("Kustomization deleted", "name", ks.Name, "namespace", ks.Namespace)
	return nil
}
