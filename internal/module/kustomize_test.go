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
	"os"
	"path/filepath"
	"testing"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	kustypes "sigs.k8s.io/kustomize/api/types"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestApplyPatches_NoPatches(t *testing.T) {
	if err := applyPatches("/nonexistent", nil); err != nil {
		t.Errorf("nil patches should be no-op: %v", err)
	}
	if err := applyPatches("/nonexistent", []modulev1alpha1.KustomizePatch{}); err != nil {
		t.Errorf("empty patches should be no-op: %v", err)
	}
}

func TestApplyPatches_MissingFile(t *testing.T) {
	patches := []modulev1alpha1.KustomizePatch{
		{Patch: "some-patch"},
	}

	err := applyPatches("/nonexistent-path", patches)
	if err == nil {
		t.Fatal("expected error for missing kustomization.yaml")
	}
}

func TestApplyPatches_InjectsPatches(t *testing.T) {
	dir := t.TempDir()
	kustomizationFile := filepath.Join(dir, "kustomization.yaml")

	initial := kustypes.Kustomization{
		TypeMeta: kustypes.TypeMeta{
			APIVersion: "kustomize.config.k8s.io/v1beta1",
			Kind:       "Kustomization",
		},
	}
	initial.Resources = []string{"deployment.yaml"}
	data, err := sigsyaml.Marshal(initial)
	if err != nil {
		t.Fatalf("marshalling initial kustomization: %v", err)
	}
	if err := os.WriteFile(kustomizationFile, data, 0o644); err != nil {
		t.Fatalf("writing kustomization.yaml: %v", err)
	}

	patches := []modulev1alpha1.KustomizePatch{
		{
			Patch: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 5
`,
		},
		{
			Patch: `
- op: replace
  path: /spec/replicas
  value: 10
`,
			Target: &modulev1alpha1.PatchSelector{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
				Name:    "web",
			},
		},
	}

	if err := applyPatches(dir, patches); err != nil {
		t.Fatalf("applyPatches failed: %v", err)
	}

	result, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	var kust kustypes.Kustomization
	if err := sigsyaml.Unmarshal(result, &kust); err != nil {
		t.Fatalf("parsing result: %v", err)
	}

	if len(kust.Patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(kust.Patches))
	}

	if kust.Patches[1].Target == nil {
		t.Fatal("second patch should have a target")
	}
	if kust.Patches[1].Target.Kind != "Deployment" {
		t.Errorf("target kind: got %q, want Deployment", kust.Patches[1].Target.Kind)
	}

	if len(kust.Resources) != 1 || kust.Resources[0] != "deployment.yaml" {
		t.Errorf("resources should be preserved: got %v", kust.Resources)
	}
}

func TestApplyPatches_PreservesExistingPatches(t *testing.T) {
	dir := t.TempDir()
	kustomizationFile := filepath.Join(dir, "kustomization.yaml")

	initial := kustypes.Kustomization{
		TypeMeta: kustypes.TypeMeta{
			APIVersion: "kustomize.config.k8s.io/v1beta1",
			Kind:       "Kustomization",
		},
	}
	initial.Patches = []kustypes.Patch{
		{Patch: "existing-patch-content"},
	}
	data, _ := sigsyaml.Marshal(initial)
	_ = os.WriteFile(kustomizationFile, data, 0o644)

	patches := []modulev1alpha1.KustomizePatch{
		{Patch: "new-patch-content"},
	}

	if err := applyPatches(dir, patches); err != nil {
		t.Fatalf("applyPatches failed: %v", err)
	}

	result, _ := os.ReadFile(kustomizationFile)
	var kust kustypes.Kustomization
	_ = sigsyaml.Unmarshal(result, &kust)

	if len(kust.Patches) != 2 {
		t.Fatalf("expected 2 patches (1 existing + 1 new), got %d", len(kust.Patches))
	}
	if kust.Patches[0].Patch != "existing-patch-content" {
		t.Errorf("first patch should be existing: got %q", kust.Patches[0].Patch)
	}
	if kust.Patches[1].Patch != "new-patch-content" {
		t.Errorf("second patch should be new: got %q", kust.Patches[1].Patch)
	}
}
