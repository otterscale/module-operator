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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestInventoryID_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		objName   string
		group     string
		kind      string
	}{
		{"simple namespaced", "default", "my-deploy", "apps", "Deployment"},
		{"cluster-scoped", "", "my-crd", "apiextensions.k8s.io", "CustomResourceDefinition"},
		{"dotted group", "prod", "my-svc", "networking.k8s.io", "Ingress"},
		{"empty group (core)", "kube-system", "my-configmap", "", "ConfigMap"},
		{"hyphenated name", "my-ns", "my-cool-app", "apps", "StatefulSet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := inventoryID(tt.namespace, tt.objName, tt.group, tt.kind)
			gotNS, gotName, gotGroup, gotKind := parseInventoryID(id)

			if gotNS != tt.namespace {
				t.Errorf("namespace: got %q, want %q", gotNS, tt.namespace)
			}
			if gotName != tt.objName {
				t.Errorf("name: got %q, want %q", gotName, tt.objName)
			}
			if gotGroup != tt.group {
				t.Errorf("group: got %q, want %q", gotGroup, tt.group)
			}
			if gotKind != tt.kind {
				t.Errorf("kind: got %q, want %q", gotKind, tt.kind)
			}
		})
	}
}

func TestParseInventoryID_LegacyFormat(t *testing.T) {
	legacyID := "default_my-deploy_apps_Deployment"
	ns, name, group, kind := parseInventoryID(legacyID)

	if ns != "default" {
		t.Errorf("namespace: got %q, want %q", ns, "default")
	}
	if name != "my-deploy" {
		t.Errorf("name: got %q, want %q", name, "my-deploy")
	}
	if group != "apps" {
		t.Errorf("group: got %q, want %q", group, "apps")
	}
	if kind != "Deployment" {
		t.Errorf("kind: got %q, want %q", kind, "Deployment")
	}
}

func TestParseInventoryID_InvalidFormat(t *testing.T) {
	_, name, group, kind := parseInventoryID("just-a-name")

	if name != "just-a-name" {
		t.Errorf("name: got %q, want %q", name, "just-a-name")
	}
	if group != "" {
		t.Errorf("group: got %q, want empty", group)
	}
	if kind != "" {
		t.Errorf("kind: got %q, want empty", kind)
	}
}

func TestBuildInventory(t *testing.T) {
	objects := []*unstructured.Unstructured{
		makeUnstructured("apps/v1", "Deployment", "default", "web"),
		makeUnstructured("v1", "Service", "default", "web-svc"),
		makeUnstructured("v1", "ConfigMap", "", "global-cfg"),
	}

	entries := buildInventory(objects)

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	ns, name, _, kind := parseInventoryID(entries[0].ID)
	if ns != "default" || name != "web" || kind != "Deployment" {
		t.Errorf("entry[0]: ns=%q name=%q kind=%q", ns, name, kind)
	}
	if entries[0].Version != "apps/v1" {
		t.Errorf("entry[0].Version: got %q, want %q", entries[0].Version, "apps/v1")
	}

	ns, name, _, kind = parseInventoryID(entries[2].ID)
	if ns != "" || name != "global-cfg" || kind != "ConfigMap" {
		t.Errorf("entry[2]: ns=%q name=%q kind=%q", ns, name, kind)
	}
}

func TestBuildInventory_Empty(t *testing.T) {
	entries := buildInventory(nil)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestDiffInventory(t *testing.T) {
	old := []modulev1alpha1.InventoryEntry{
		{ID: inventoryID("default", "a", "apps", "Deployment"), Version: "apps/v1"},
		{ID: inventoryID("default", "b", "apps", "Deployment"), Version: "apps/v1"},
		{ID: inventoryID("default", "c", "", "Service"), Version: "v1"},
	}
	current := []modulev1alpha1.InventoryEntry{
		{ID: inventoryID("default", "a", "apps", "Deployment"), Version: "apps/v1"},
		{ID: inventoryID("default", "d", "", "ConfigMap"), Version: "v1"},
	}

	stale := diffInventory(old, current)

	if len(stale) != 2 {
		t.Fatalf("expected 2 stale, got %d", len(stale))
	}

	ids := map[string]bool{}
	for _, e := range stale {
		ids[e.ID] = true
	}
	bID := inventoryID("default", "b", "apps", "Deployment")
	cID := inventoryID("default", "c", "", "Service")
	if !ids[bID] {
		t.Error("missing stale entry for b")
	}
	if !ids[cID] {
		t.Error("missing stale entry for c")
	}
}

func TestDiffInventory_NilInputs(t *testing.T) {
	stale := diffInventory(nil, nil)
	if len(stale) != 0 {
		t.Errorf("expected 0 stale, got %d", len(stale))
	}

	stale = diffInventory(nil, []modulev1alpha1.InventoryEntry{{ID: "x"}})
	if len(stale) != 0 {
		t.Errorf("expected 0 stale, got %d", len(stale))
	}
}

func TestDiffInventory_AllStale(t *testing.T) {
	old := []modulev1alpha1.InventoryEntry{
		{ID: inventoryID("ns", "a", "", "Service"), Version: "v1"},
	}
	stale := diffInventory(old, nil)

	if len(stale) != 1 {
		t.Fatalf("expected 1 stale, got %d", len(stale))
	}
}

func TestInventoryGVK(t *testing.T) {
	entry := modulev1alpha1.InventoryEntry{
		ID:      inventoryID("default", "web", "apps", "Deployment"),
		Version: "apps/v1",
	}

	gvk := inventoryGVK(entry)
	want := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	if gvk != want {
		t.Errorf("got %v, want %v", gvk, want)
	}
}

func TestInventoryGVK_CoreGroup(t *testing.T) {
	entry := modulev1alpha1.InventoryEntry{
		ID:      inventoryID("default", "my-svc", "", "Service"),
		Version: "v1",
	}

	gvk := inventoryGVK(entry)
	want := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}

	if gvk != want {
		t.Errorf("got %v, want %v", gvk, want)
	}
}

func makeUnstructured(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}
