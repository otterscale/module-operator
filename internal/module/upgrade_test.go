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

package module_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/otterscale/addons-operator/internal/module"
	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

var _ = Describe("CheckUpgrade", func() {

	Describe("initial install detection", func() {
		It("returns UpgradeInitialInstall when appliedTemplateGeneration is zero", func() {
			m := &addonsv1alpha1.Module{
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 0,
				},
			}
			mt := &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
			}

			Expect(module.CheckUpgrade(m, mt)).To(Equal(module.UpgradeInitialInstall))
		})
	})

	Describe("no-change detection", func() {
		It("returns UpgradeNotNeeded when template generation equals applied", func() {
			m := &addonsv1alpha1.Module{
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 3,
				},
			}
			mt := &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
			}

			Expect(module.CheckUpgrade(m, mt)).To(Equal(module.UpgradeNotNeeded))
		})
	})

	Describe("auto-approve (backward compatible)", func() {
		It("returns UpgradeApproved when approvedTemplateGeneration is nil", func() {
			m := &addonsv1alpha1.Module{
				Spec: addonsv1alpha1.ModuleSpec{
					ApprovedTemplateGeneration: nil,
				},
				Status: addonsv1alpha1.ModuleStatus{
					AppliedTemplateGeneration: 1,
				},
			}
			mt := &addonsv1alpha1.ModuleTemplate{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
			}

			Expect(module.CheckUpgrade(m, mt)).To(Equal(module.UpgradeApproved))
		})
	})

	Describe("explicit approval", func() {
		DescribeTable("returns UpgradeApproved when approved generation covers the template",
			func(approved int64, templateGen int64) {
				m := &addonsv1alpha1.Module{
					Spec: addonsv1alpha1.ModuleSpec{
						ApprovedTemplateGeneration: new(approved),
					},
					Status: addonsv1alpha1.ModuleStatus{
						AppliedTemplateGeneration: 3,
					},
				}
				mt := &addonsv1alpha1.ModuleTemplate{
					ObjectMeta: metav1.ObjectMeta{Generation: templateGen},
				}

				Expect(module.CheckUpgrade(m, mt)).To(Equal(module.UpgradeApproved))
			},
			Entry("exact match", int64(5), int64(5)),
			Entry("pre-approved (exceeds template generation)", int64(10), int64(5)),
		)
	})

	Describe("pending approval", func() {
		DescribeTable("returns UpgradePending when approved generation is insufficient",
			func(approved int64, applied int64, templateGen int64) {
				m := &addonsv1alpha1.Module{
					Spec: addonsv1alpha1.ModuleSpec{
						ApprovedTemplateGeneration: new(approved),
					},
					Status: addonsv1alpha1.ModuleStatus{
						AppliedTemplateGeneration: applied,
					},
				}
				mt := &addonsv1alpha1.ModuleTemplate{
					ObjectMeta: metav1.ObjectMeta{Generation: templateGen},
				}

				Expect(module.CheckUpgrade(m, mt)).To(Equal(module.UpgradePending))
			},
			Entry("approved below template generation", int64(2), int64(2), int64(3)),
			Entry("approved explicitly set to zero", int64(0), int64(1), int64(2)),
		)
	})
})

var _ = Describe("UpgradeDecision.ShouldApply", func() {
	DescribeTable("returns whether the decision permits applying template changes",
		func(decision module.UpgradeDecision, expected bool) {
			Expect(decision.ShouldApply()).To(Equal(expected))
		},
		Entry("UpgradeNotNeeded permits apply", module.UpgradeNotNeeded, true),
		Entry("UpgradeInitialInstall permits apply", module.UpgradeInitialInstall, true),
		Entry("UpgradeApproved permits apply", module.UpgradeApproved, true),
		Entry("UpgradePending blocks apply", module.UpgradePending, false),
	)
})
