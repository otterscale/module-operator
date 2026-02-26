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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

func newInt64(v int) *int64 {
	int64 := int64(v)
	return &int64
}

func TestCheckUpgrade(t *testing.T) {
	tests := []struct {
		name     string
		module   *addonsv1alpha1.Module
		template *addonsv1alpha1.ModuleTemplate
		want     UpgradeDecision
	}{
		{
			name: "initial install — appliedTemplateGeneration is zero",
			module: &addonsv1alpha1.Module{
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 0,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
			},
			want: UpgradeInitialInstall,
		},
		{
			name: "no change — template generation equals applied",
			module: &addonsv1alpha1.Module{
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 3,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
			},
			want: UpgradeNotNeeded,
		},
		{
			name: "auto-approve — approvedTemplateGeneration is nil (backward compatible)",
			module: &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: nil,
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 1,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
			},
			want: UpgradeApproved,
		},
		{
			name: "approved — approvedTemplateGeneration matches template generation",
			module: &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: newInt64(5),
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 3,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
			},
			want: UpgradeApproved,
		},
		{
			name: "approved — approvedTemplateGeneration exceeds template generation (pre-approved)",
			module: &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: newInt64(10),
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 3,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
			},
			want: UpgradeApproved,
		},
		{
			name: "pending — approvedTemplateGeneration below template generation",
			module: &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: newInt64(2),
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 2,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
			},
			want: UpgradePending,
		},
		{
			name: "pending — approvedTemplateGeneration is zero (explicitly set to 0)",
			module: &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: newInt64(0),
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 1,
				},
			},
			template: &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
			},
			want: UpgradePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckUpgrade(tt.module, tt.template)
			if got != tt.want {
				t.Errorf("CheckUpgrade() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestUpgradeDecision_ShouldApply(t *testing.T) {
	tests := []struct {
		decision UpgradeDecision
		want     bool
	}{
		{UpgradeNotNeeded, true},
		{UpgradeInitialInstall, true},
		{UpgradeApproved, true},
		{UpgradePending, false},
	}

	for _, tt := range tests {
		if got := tt.decision.ShouldApply(); got != tt.want {
			t.Errorf("UpgradeDecision(%d).ShouldApply() = %v, want %v", tt.decision, got, tt.want)
		}
	}
}
