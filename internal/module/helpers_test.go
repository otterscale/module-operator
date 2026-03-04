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
	"testing"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	"github.com/otterscale/module-operator/internal/labels"
)

func TestTargetNamespace_ModuleOverride(t *testing.T) {
	override := "module-ns"
	m := &modulev1alpha1.Module{
		Spec: modulev1alpha1.ModuleSpec{Namespace: &override},
	}
	mc := &modulev1alpha1.ModuleClass{
		Spec: modulev1alpha1.ModuleClassSpec{Namespace: "class-ns"},
	}

	if got := TargetNamespace(m, mc); got != "module-ns" {
		t.Errorf("got %q, want %q", got, "module-ns")
	}
}

func TestTargetNamespace_ClassDefault(t *testing.T) {
	m := &modulev1alpha1.Module{}
	mc := &modulev1alpha1.ModuleClass{
		Spec: modulev1alpha1.ModuleClassSpec{Namespace: "class-ns"},
	}

	if got := TargetNamespace(m, mc); got != "class-ns" {
		t.Errorf("got %q, want %q", got, "class-ns")
	}
}

func TestLabelsForModule(t *testing.T) {
	l := LabelsForModule("my-module", "my-class", "v1.0.0")

	if l[labels.Name] != "my-module" {
		t.Errorf("name label: got %q", l[labels.Name])
	}
	if l[labels.Component] != "module" {
		t.Errorf("component label: got %q", l[labels.Component])
	}
	if l[labels.PartOf] != labels.System {
		t.Errorf("partOf label: got %q", l[labels.PartOf])
	}
	if l[labels.ManagedBy] != labels.Operator {
		t.Errorf("managedBy label: got %q", l[labels.ManagedBy])
	}
	if l[labels.Version] != "v1.0.0" {
		t.Errorf("version label: got %q", l[labels.Version])
	}
	if l[LabelModuleClass] != "my-class" {
		t.Errorf("module-class label: got %q", l[LabelModuleClass])
	}
}

func TestLabelsForModule_EmptyVersion(t *testing.T) {
	l := LabelsForModule("my-module", "my-class", "")

	if _, ok := l[labels.Version]; ok {
		t.Error("version label should be omitted when empty")
	}
	if l[LabelModuleClass] != "my-class" {
		t.Errorf("module-class label: got %q", l[LabelModuleClass])
	}
}

func TestClassNotFoundError(t *testing.T) {
	err := &ClassNotFoundError{Name: "test-class"}
	want := `moduleclass "test-class" not found`
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

func TestClassInvalidError(t *testing.T) {
	err := &ClassInvalidError{Name: "bad-class", Message: "missing config"}
	want := `moduleclass "bad-class" is invalid: missing config`
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}
