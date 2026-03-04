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

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	"github.com/otterscale/module-operator/internal/module"
)

var _ = Describe("CheckUpgrade", func() {

	Describe("initial install detection", func() {
		It("returns UpgradeInitialInstall when appliedClassGeneration is zero", func() {
			m := &modulev1alpha1.Module{
				Status: modulev1alpha1.ModuleStatus{
					AppliedClassGeneration: 0,
				},
			}
			mc := &modulev1alpha1.ModuleClass{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
			}

			Expect(module.CheckUpgrade(m, mc)).To(Equal(module.UpgradeInitialInstall))
		})
	})

	Describe("no-change detection", func() {
		It("returns UpgradeNotNeeded when class generation equals applied", func() {
			m := &modulev1alpha1.Module{
				Status: modulev1alpha1.ModuleStatus{
					AppliedClassGeneration: 3,
				},
			}
			mc := &modulev1alpha1.ModuleClass{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
			}

			Expect(module.CheckUpgrade(m, mc)).To(Equal(module.UpgradeNotNeeded))
		})
	})

	Describe("auto-approve (backward compatible)", func() {
		It("returns UpgradeApproved when approvedClassGeneration is nil", func() {
			m := &modulev1alpha1.Module{
				Spec: modulev1alpha1.ModuleSpec{
					ApprovedClassGeneration: nil,
				},
				Status: modulev1alpha1.ModuleStatus{
					AppliedClassGeneration: 1,
				},
			}
			mc := &modulev1alpha1.ModuleClass{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
			}

			Expect(module.CheckUpgrade(m, mc)).To(Equal(module.UpgradeApproved))
		})
	})

	Describe("explicit approval", func() {
		DescribeTable("returns UpgradeApproved when approved generation covers the class",
			func(approved int64, classGen int64) {
				m := &modulev1alpha1.Module{
					Spec: modulev1alpha1.ModuleSpec{
						ApprovedClassGeneration: new(approved),
					},
					Status: modulev1alpha1.ModuleStatus{
						AppliedClassGeneration: 3,
					},
				}
				mc := &modulev1alpha1.ModuleClass{
					ObjectMeta: metav1.ObjectMeta{Generation: classGen},
				}

				Expect(module.CheckUpgrade(m, mc)).To(Equal(module.UpgradeApproved))
			},
			Entry("exact match", int64(5), int64(5)),
			Entry("pre-approved (exceeds class generation)", int64(10), int64(5)),
		)
	})

	Describe("pending approval", func() {
		DescribeTable("returns UpgradePending when approved generation is insufficient",
			func(approved int64, applied int64, classGen int64) {
				m := &modulev1alpha1.Module{
					Spec: modulev1alpha1.ModuleSpec{
						ApprovedClassGeneration: new(approved),
					},
					Status: modulev1alpha1.ModuleStatus{
						AppliedClassGeneration: applied,
					},
				}
				mc := &modulev1alpha1.ModuleClass{
					ObjectMeta: metav1.ObjectMeta{Generation: classGen},
				}

				Expect(module.CheckUpgrade(m, mc)).To(Equal(module.UpgradePending))
			},
			Entry("approved below class generation", int64(2), int64(2), int64(3)),
			Entry("approved explicitly set to zero", int64(0), int64(1), int64(2)),
		)
	})
})

var _ = Describe("UpgradeDecision.ShouldApply", func() {
	DescribeTable("returns whether the decision permits applying class changes",
		func(decision module.UpgradeDecision, expected bool) {
			Expect(decision.ShouldApply()).To(Equal(expected))
		},
		Entry("UpgradeNotNeeded permits apply", module.UpgradeNotNeeded, true),
		Entry("UpgradeInitialInstall permits apply", module.UpgradeInitialInstall, true),
		Entry("UpgradeApproved permits apply", module.UpgradeApproved, true),
		Entry("UpgradePending blocks apply", module.UpgradePending, false),
	)
})
