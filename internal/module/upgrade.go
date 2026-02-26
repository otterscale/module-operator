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
	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

// UpgradeDecision represents the outcome of evaluating whether a Module
// should apply changes from its referenced ModuleTemplate.
type UpgradeDecision int

const (
	// UpgradeNotNeeded indicates the template has not changed since the last apply.
	UpgradeNotNeeded UpgradeDecision = iota

	// UpgradeInitialInstall indicates this is the first reconciliation and
	// the template should be applied without requiring explicit approval.
	UpgradeInitialInstall

	// UpgradeApproved indicates the template has changed and the user has
	// approved the new generation (or approval is not configured).
	UpgradeApproved

	// UpgradePending indicates the template has changed but the user has
	// not yet approved the new generation.
	UpgradePending
)

// CheckUpgrade is a pure function that determines whether a Module should
// apply template changes based on the current state. It has no side effects
// and performs no I/O.
//
// Decision logic:
//   - AppliedTemplateGeneration == 0 → first install, always apply
//   - Template generation unchanged  → nothing to do
//   - ApprovedTemplateGeneration nil  → auto-approve (backward compatible)
//   - Approved >= template generation → approved by user
//   - Otherwise                       → pending user approval
func CheckUpgrade(m *addonsv1alpha1.Module, mt *addonsv1alpha1.ModuleTemplate) UpgradeDecision {
	if m.Status.AppliedTemplateGeneration == 0 {
		return UpgradeInitialInstall
	}
	if mt.Generation <= m.Status.AppliedTemplateGeneration {
		return UpgradeNotNeeded
	}
	if m.Spec.ApprovedTemplateGeneration == nil {
		return UpgradeApproved
	}
	if *m.Spec.ApprovedTemplateGeneration >= mt.Generation {
		return UpgradeApproved
	}
	return UpgradePending
}

// ShouldApply returns true when the decision permits applying template changes
// to the FluxCD resources.
func (d UpgradeDecision) ShouldApply() bool {
	return d != UpgradePending
}
