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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

// buildInventory creates an inventory list from a set of unstructured
// Kubernetes objects. Each entry records the object's identity so that
// future reconciliations can detect and prune removed resources.
func buildInventory(objects []*unstructured.Unstructured) []modulev1alpha1.InventoryEntry {
	entries := make([]modulev1alpha1.InventoryEntry, 0, len(objects))
	for _, obj := range objects {
		gvk := obj.GroupVersionKind()
		id := inventoryID(obj.GetNamespace(), obj.GetName(), gvk.Group, gvk.Kind)
		entries = append(entries, modulev1alpha1.InventoryEntry{
			ID:      id,
			Version: gvk.GroupVersion().String(),
		})
	}
	return entries
}

// diffInventory returns the entries present in old but absent from current,
// representing resources that should be pruned.
func diffInventory(old, current []modulev1alpha1.InventoryEntry) []modulev1alpha1.InventoryEntry {
	currentSet := make(map[string]struct{}, len(current))
	for _, e := range current {
		currentSet[e.ID] = struct{}{}
	}
	var stale []modulev1alpha1.InventoryEntry
	for _, e := range old {
		if _, ok := currentSet[e.ID]; !ok {
			stale = append(stale, e)
		}
	}
	return stale
}

// parseInventoryID splits an inventory ID back into its components.
func parseInventoryID(id string) (namespace, name, group, kind string) {
	parts := strings.SplitN(id, "_", 4)
	if len(parts) != 4 {
		return "", id, "", ""
	}
	return parts[0], parts[1], parts[2], parts[3]
}

// inventoryGVK reconstructs a GVK from an inventory entry.
func inventoryGVK(entry modulev1alpha1.InventoryEntry) schema.GroupVersionKind {
	ns, _, group, kind := parseInventoryID(entry.ID)
	_ = ns
	gv, _ := schema.ParseGroupVersion(entry.Version)
	return schema.GroupVersionKind{Group: group, Kind: kind, Version: gv.Version}
}

func inventoryID(namespace, name, group, kind string) string {
	return fmt.Sprintf("%s_%s_%s_%s", namespace, name, group, kind)
}
