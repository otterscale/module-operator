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

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

// ReconcileHelmRelease ensures the FluxCD HelmRelease exists and matches the
// desired state derived from the ModuleTemplate and Module overrides.
//
// The HelmRelease is created in the target namespace with OwnerReference
// pointing to the cluster-scoped Module for garbage collection.
func ReconcileHelmRelease(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	m *modulev1alpha1.Module,
	mt *modulev1alpha1.ModuleTemplate,
	version string,
) error {
	if mt.Spec.HelmRelease == nil {
		return &TemplateInvalidError{
			Name:    mt.Name,
			Message: "helmRelease spec is nil but Module expects a HelmRelease template",
		}
	}

	var templateSpec helmv2.HelmReleaseSpec
	if err := json.Unmarshal(mt.Spec.HelmRelease.Raw, &templateSpec); err != nil {
		return &TemplateInvalidError{
			Name:    mt.Name,
			Message: fmt.Sprintf("failed to unmarshal helmRelease spec: %v", err),
		}
	}

	targetNS := TargetNamespace(m, mt)

	if err := EnsureNamespace(ctx, c, targetNS); err != nil {
		return err
	}

	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: targetNS,
		},
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, c, hr, func() error {
		if m.Spec.Values != nil {
			templateSpec.Values = &apiextensionsv1.JSON{Raw: m.Spec.Values.Raw}
		}

		hr.Spec = templateSpec

		// Ensure labels are set for identification and filtering
		if hr.Labels == nil {
			hr.Labels = map[string]string{}
		}
		maps.Copy(hr.Labels, LabelsForModule(m.Name, mt.Name, version))

		// Set OwnerReference for garbage collection.
		// Cluster-scoped owners CAN own namespace-scoped resources in Kubernetes.
		return ctrlutil.SetControllerReference(m, hr, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("HelmRelease reconciled",
			"operation", op, "name", hr.Name, "namespace", hr.Namespace)
	}
	return nil
}

// DeleteHelmRelease deletes the FluxCD HelmRelease associated with the Module.
// It returns nil if the resource is already gone.
func DeleteHelmRelease(ctx context.Context, c client.Client, m *modulev1alpha1.Module, namespace string) error {
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: namespace,
		},
	}
	if err := c.Delete(ctx, hr); err != nil {
		return client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).Info("HelmRelease deleted", "name", hr.Name, "namespace", hr.Namespace)
	return nil
}
