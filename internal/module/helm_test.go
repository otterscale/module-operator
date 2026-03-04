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
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

func TestMergeValues_BaseOnly(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{
		Values: &runtime.RawExtension{Raw: []byte(`{"replicas":3,"image":"nginx"}`)},
	}
	m := &modulev1alpha1.Module{}

	vals, err := mergeValues(ht, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vals["replicas"] != float64(3) {
		t.Errorf("replicas: got %v, want 3", vals["replicas"])
	}
	if vals["image"] != "nginx" {
		t.Errorf("image: got %v, want nginx", vals["image"])
	}
}

func TestMergeValues_OverrideOnly(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{}
	m := &modulev1alpha1.Module{
		Spec: modulev1alpha1.ModuleSpec{
			Values: &runtime.RawExtension{Raw: []byte(`{"replicas":5}`)},
		},
	}

	vals, err := mergeValues(ht, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vals["replicas"] != float64(5) {
		t.Errorf("replicas: got %v, want 5", vals["replicas"])
	}
}

func TestMergeValues_DeepMerge(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{
		Values: &runtime.RawExtension{Raw: []byte(`{
			"app": {"name":"web","port":8080},
			"replicas": 1
		}`)},
	}
	m := &modulev1alpha1.Module{
		Spec: modulev1alpha1.ModuleSpec{
			Values: &runtime.RawExtension{Raw: []byte(`{
				"app": {"port":9090,"debug":true},
				"replicas": 3
			}`)},
		},
	}

	vals, err := mergeValues(ht, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	app, ok := vals["app"].(map[string]any)
	if !ok {
		t.Fatalf("app is not a map: %T", vals["app"])
	}
	if app["name"] != "web" {
		t.Errorf("app.name: got %v, want web (should be preserved from base)", app["name"])
	}
	if app["port"] != float64(9090) {
		t.Errorf("app.port: got %v, want 9090 (should be overridden)", app["port"])
	}
	if app["debug"] != true {
		t.Errorf("app.debug: got %v, want true (should be added from override)", app["debug"])
	}
	if vals["replicas"] != float64(3) {
		t.Errorf("replicas: got %v, want 3 (should be overridden)", vals["replicas"])
	}
}

func TestMergeValues_BothNil(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{}
	m := &modulev1alpha1.Module{}

	vals, err := mergeValues(ht, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("expected empty map, got %v", vals)
	}
}

func TestMergeValues_InvalidBaseJSON(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{
		Values: &runtime.RawExtension{Raw: []byte(`{invalid}`)},
	}
	m := &modulev1alpha1.Module{}

	_, err := mergeValues(ht, m)
	if err == nil {
		t.Fatal("expected error for invalid base JSON")
	}
}

func TestMergeValues_InvalidOverrideJSON(t *testing.T) {
	ht := &modulev1alpha1.HelmChartTemplate{
		Values: &runtime.RawExtension{Raw: []byte(`{"a":1}`)},
	}
	m := &modulev1alpha1.Module{
		Spec: modulev1alpha1.ModuleSpec{
			Values: &runtime.RawExtension{Raw: []byte(`not-json`)},
		},
	}

	_, err := mergeValues(ht, m)
	if err == nil {
		t.Fatal("expected error for invalid override JSON")
	}
}

func TestMergeMaps_OverrideScalar(t *testing.T) {
	base := map[string]any{"a": 1, "b": "hello"}
	override := map[string]any{"a": 2}

	result := mergeMaps(base, override)

	if result["a"] != 2 {
		t.Errorf("a: got %v, want 2", result["a"])
	}
	if result["b"] != "hello" {
		t.Errorf("b: got %v, want hello", result["b"])
	}
}

func TestMergeMaps_NestedRecursive(t *testing.T) {
	base := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"keep": "yes",
				"old":  "value",
			},
		},
	}
	override := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"old": "new-value",
				"new": "added",
			},
		},
	}

	result := mergeMaps(base, override)

	l1, _ := result["level1"].(map[string]any)
	l2, _ := l1["level2"].(map[string]any)

	if l2["keep"] != "yes" {
		t.Errorf("keep: got %v, want yes", l2["keep"])
	}
	if l2["old"] != "new-value" {
		t.Errorf("old: got %v, want new-value", l2["old"])
	}
	if l2["new"] != "added" {
		t.Errorf("new: got %v, want added", l2["new"])
	}
}

func TestMergeMaps_OverrideMapWithScalar(t *testing.T) {
	base := map[string]any{
		"config": map[string]any{"key": "val"},
	}
	override := map[string]any{
		"config": "replaced-with-string",
	}

	result := mergeMaps(base, override)

	if result["config"] != "replaced-with-string" {
		t.Errorf("config: got %v, want replaced-with-string", result["config"])
	}
}

func TestMergeMaps_DoesNotMutateBase(t *testing.T) {
	base := map[string]any{"a": 1, "b": 2}
	override := map[string]any{"a": 99}

	_ = mergeMaps(base, override)

	if base["a"] != 1 {
		t.Errorf("base was mutated: a = %v", base["a"])
	}
}

func TestComputeValuesChecksum_Deterministic(t *testing.T) {
	vals := map[string]any{"a": 1, "b": "hello", "c": []any{1, 2, 3}}

	c1, err := computeValuesChecksum(vals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c2, err := computeValuesChecksum(vals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c1 != c2 {
		t.Errorf("checksums differ: %s vs %s", c1, c2)
	}
	if len(c1) != 64 {
		t.Errorf("expected 64-char hex SHA256, got length %d", len(c1))
	}
}

func TestComputeValuesChecksum_DifferentValues(t *testing.T) {
	c1, _ := computeValuesChecksum(map[string]any{"a": 1})
	c2, _ := computeValuesChecksum(map[string]any{"a": 2})

	if c1 == c2 {
		t.Error("different values produced same checksum")
	}
}

func TestComputeValuesChecksum_Empty(t *testing.T) {
	checksum, err := computeValuesChecksum(map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := json.Marshal(map[string]any{})
	if len(checksum) != 64 || checksum == "" {
		t.Errorf("invalid checksum for empty map: %q (data: %s)", checksum, data)
	}
}
