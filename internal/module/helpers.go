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
	"fmt"

	"github.com/otterscale/addons-operator/internal/labels"
	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

const (
	// ModuleFinalizer is set on a Module when it is first handled by the controller.
	// It ensures that FluxCD resources are properly cleaned up before the Module is deleted.
	ModuleFinalizer = "addons.otterscale.io/module-cleanup"

	// ConditionTypeReady indicates whether the module's FluxCD resources
	// have been successfully reconciled and are healthy.
	ConditionTypeReady = "Ready"

	// ConditionTypeUpgradeAvailable indicates that a newer ModuleTemplate
	// generation exists but has not yet been approved for deployment.
	ConditionTypeUpgradeAvailable = "UpgradeAvailable"

	// LabelModuleTemplate identifies the ModuleTemplate that this resource was created from.
	LabelModuleTemplate = "addons.otterscale.io/module-template"
)

// TemplateNotFoundError is a permanent error indicating the referenced
// ModuleTemplate does not exist.
type TemplateNotFoundError struct {
	Name string
}

func (e *TemplateNotFoundError) Error() string {
	return fmt.Sprintf("moduletemplate %q not found", e.Name)
}

// TemplateInvalidError is a permanent error indicating the referenced
// ModuleTemplate has an invalid configuration (e.g. neither helmRelease nor kustomization is set).
type TemplateInvalidError struct {
	Name    string
	Message string
}

func (e *TemplateInvalidError) Error() string {
	return fmt.Sprintf("moduletemplate %q is invalid: %s", e.Name, e.Message)
}

// LabelsForModule returns a standard set of labels for resources managed by a Module.
// It builds on the shared labels.Standard() base and adds the Module-specific template label.
func LabelsForModule(moduleName, templateName, version string) map[string]string {
	l := labels.Standard(moduleName, "module", version)
	l[LabelModuleTemplate] = templateName
	return l
}

// TargetNamespace resolves the effective namespace for a Module,
// preferring the Module's override, falling back to the ModuleTemplate default.
func TargetNamespace(m *addonsv1alpha1.Module, mt *addonsv1alpha1.ModuleTemplate) string {
	if m.Spec.Namespace != nil {
		return *m.Spec.Namespace
	}
	return mt.Spec.Namespace
}
