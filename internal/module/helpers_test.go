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

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	"github.com/otterscale/module-operator/internal/labels"
	"github.com/otterscale/module-operator/internal/module"
)

var _ = Describe("LabelsForModule", func() {
	It("includes all standard labels plus module-template", func() {
		l := module.LabelsForModule("my-mod", "my-tmpl", "v1.0.0")

		Expect(l).To(HaveKeyWithValue(labels.Name, "my-mod"))
		Expect(l).To(HaveKeyWithValue(labels.Component, "module"))
		Expect(l).To(HaveKeyWithValue(labels.ManagedBy, labels.Operator))
		Expect(l).To(HaveKeyWithValue(labels.PartOf, labels.System))
		Expect(l).To(HaveKeyWithValue(labels.Version, "v1.0.0"))
		Expect(l).To(HaveKeyWithValue(module.LabelModuleTemplate, "my-tmpl"))
	})

	It("omits version label when version is empty", func() {
		l := module.LabelsForModule("mod", "tmpl", "")

		Expect(l).NotTo(HaveKey(labels.Version))
		Expect(l).To(HaveKeyWithValue(module.LabelModuleTemplate, "tmpl"))
	})
})

var _ = Describe("TargetNamespace", func() {
	var mt *modulev1alpha1.ModuleTemplate

	BeforeEach(func() {
		mt = &modulev1alpha1.ModuleTemplate{
			Spec: modulev1alpha1.ModuleTemplateSpec{Namespace: "template-ns"},
		}
	})

	It("returns the template namespace when Module has no override", func() {
		m := &modulev1alpha1.Module{}
		Expect(module.TargetNamespace(m, mt)).To(Equal("template-ns"))
	})

	It("returns the Module override when specified", func() {
		ns := "override-ns"
		m := &modulev1alpha1.Module{
			Spec: modulev1alpha1.ModuleSpec{Namespace: &ns},
		}
		Expect(module.TargetNamespace(m, mt)).To(Equal("override-ns"))
	})

	It("prefers the Module override over template default", func() {
		ns := "user-ns"
		m := &modulev1alpha1.Module{
			Spec: modulev1alpha1.ModuleSpec{Namespace: &ns},
		}
		mt.Spec.Namespace = "template-default"
		Expect(module.TargetNamespace(m, mt)).To(Equal("user-ns"))
	})
})

var _ = Describe("TemplateNotFoundError", func() {
	It("formats the error message with the template name", func() {
		err := &module.TemplateNotFoundError{Name: "my-template"}
		Expect(err.Error()).To(Equal(`moduletemplate "my-template" not found`))
	})

	It("implements the error interface", func() {
		var err error = &module.TemplateNotFoundError{Name: "x"}
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("TemplateInvalidError", func() {
	It("formats the error message with name and detail", func() {
		err := &module.TemplateInvalidError{
			Name:    "bad-template",
			Message: "missing spec",
		}
		Expect(err.Error()).To(Equal(`moduletemplate "bad-template" is invalid: missing spec`))
	})

	It("implements the error interface", func() {
		var err error = &module.TemplateInvalidError{Name: "x", Message: "y"}
		Expect(err).To(HaveOccurred())
	})
})
