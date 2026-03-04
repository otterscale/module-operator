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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mod "github.com/otterscale/addons-operator/internal/module"
	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

var _ = Describe("Module Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
		version  = "test-v1"
	)

	var (
		reconciler     *ModuleReconciler
		module         *addonsv1alpha1.Module
		moduleTemplate *addonsv1alpha1.ModuleTemplate
		moduleName     string
		templateName   string
		targetNS       string
	)

	makeHelmChartTemplate := func(name, namespace string) *addonsv1alpha1.ModuleTemplate {
		return &addonsv1alpha1.ModuleTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: addonsv1alpha1.ModuleTemplateSpec{
				Description: "Test Helm module template",
				Namespace:   namespace,
				HelmChart: &addonsv1alpha1.HelmChartTemplate{
					RepoURL:  "https://charts.example.com",
					Chart:    "test-chart",
					Version:  "1.0.0",
					Interval: metav1.Duration{Duration: 10 * time.Minute},
				},
			},
		}
	}

	makeKustomizationTemplate := func(name, namespace string) *addonsv1alpha1.ModuleTemplate {
		return &addonsv1alpha1.ModuleTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: addonsv1alpha1.ModuleTemplateSpec{
				Description: "Test Kustomization module template",
				Namespace:   namespace,
				Kustomization: &addonsv1alpha1.KustomizationTemplate{
					URL:      "https://github.com/example/repo.git",
					Path:     "./deploy",
					Interval: metav1.Duration{Duration: 10 * time.Minute},
					Prune:    true,
				},
			},
		}
	}

	makeModule := func(name, templateRef string, mods ...func(*addonsv1alpha1.Module)) *addonsv1alpha1.Module {
		m := &addonsv1alpha1.Module{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: addonsv1alpha1.ModuleSpec{
				TemplateRef: templateRef,
			},
		}
		for _, fn := range mods {
			fn(m)
		}
		return m
	}

	ptrInt64 := func(v int64) *int64 { return &v }

	fetchResource := func(obj client.Object, name, namespace string) {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, key, obj)
		}, timeout, interval).Should(Succeed())
	}

	BeforeEach(func() {
		moduleName = "mod-" + string(uuid.NewUUID())[:8]
		templateName = "tmpl-" + string(uuid.NewUUID())[:8]
		targetNS = "ns-" + string(uuid.NewUUID())[:8]
		module = nil
		moduleTemplate = nil
		reconciler = &ModuleReconciler{
			Client:     k8sClient,
			Scheme:     k8sClient.Scheme(),
			RestConfig: cfg,
			Version:    version,
			Recorder:   events.NewFakeRecorder(100),
		}

		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: targetNS},
		})).To(Succeed())
	})

	JustBeforeEach(func() {
		if moduleTemplate != nil {
			Expect(k8sClient.Create(ctx, moduleTemplate)).To(Succeed())
		}
		if module != nil {
			Expect(k8sClient.Create(ctx, module)).To(Succeed())
		}
	})

	AfterEach(func() {
		var m addonsv1alpha1.Module
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: moduleName}, &m); err == nil {
			if ctrlutil.ContainsFinalizer(&m, mod.ModuleFinalizer) {
				patch := client.MergeFrom(m.DeepCopy())
				ctrlutil.RemoveFinalizer(&m, mod.ModuleFinalizer)
				_ = k8sClient.Patch(ctx, &m, patch)
			}
			_ = k8sClient.Delete(ctx, &m)
		}

		if moduleTemplate != nil {
			_ = k8sClient.Delete(ctx, moduleTemplate)
		}
	})

	Context("Template Not Found", func() {
		BeforeEach(func() {
			moduleTemplate = nil
			module = makeModule(moduleName, "non-existent-template")
		})

		It("should set Ready=False with TemplateNotFound and not return error", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			fetchResource(module, moduleName, "")
			readyCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("TemplateNotFound"))
		})
	})

	Context("Finalizer Management", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should add finalizer to the Module on first reconcile", func() {
			nsName := types.NamespacedName{Name: moduleName}
			// First reconcile adds finalizer. It will then fail on actual Helm
			// install (no real chart server) but finalizer is added before that.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})

			fetchResource(module, moduleName, "")
			Expect(ctrlutil.ContainsFinalizer(module, mod.ModuleFinalizer)).To(BeTrue())
		})
	})

	Context("Reconcile with HelmChart template", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should return a transient error when chart fetch fails (no real repo)", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).To(HaveOccurred())

			fetchResource(module, moduleName, "")
			readyCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("ChartFetchError"))
		})
	})

	Context("Reconcile with Kustomization template", func() {
		BeforeEach(func() {
			moduleTemplate = makeKustomizationTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should return a transient error when git clone fails (no real repo)", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).To(HaveOccurred())

			fetchResource(module, moduleName, "")
			readyCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("SourceFetchError"))
		})
	})

	Context("Template Generation Tracking with failed reconcile", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should track observed generation even when reconcile fails", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})

			fetchResource(module, moduleName, "")
			Expect(module.Status.ObservedGeneration).To(Equal(module.Generation))
		})
	})

	Context("Namespace Override", func() {
		var overrideNS string

		BeforeEach(func() {
			overrideNS = "override-" + string(uuid.NewUUID())[:8]

			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: overrideNS},
			})).To(Succeed())

			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName, func(m *addonsv1alpha1.Module) {
				m.Spec.Namespace = &overrideNS
			})
		})

		It("should resolve target namespace from Module override", func() {
			Expect(mod.TargetNamespace(module, moduleTemplate)).To(Equal(overrideNS))
		})
	})

	Context("Upgrade Pending Logic", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should detect upgrade pending when approved generation is behind", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})

			fetchResource(module, moduleName, "")

			By("Locking the Module to require approval at current generation")
			fetchResource(moduleTemplate, templateName, "")
			currentGen := moduleTemplate.Generation

			patch := client.MergeFrom(module.DeepCopy())
			module.Spec.ApprovedTemplateGeneration = ptrInt64(currentGen)
			Expect(k8sClient.Patch(ctx, module, patch)).To(Succeed())

			By("Updating the ModuleTemplate to create a newer generation")
			fetchResource(moduleTemplate, templateName, "")
			moduleTemplate.Spec.Description = "Updated description"
			Expect(k8sClient.Update(ctx, moduleTemplate)).To(Succeed())

			fetchResource(moduleTemplate, templateName, "")
			Expect(moduleTemplate.Generation).To(BeNumerically(">", currentGen))

			By("Verifying CheckUpgrade reports UpgradePending")
			fetchResource(module, moduleName, "")
			module.Status.AppliedTemplateGeneration = currentGen
			decision := mod.CheckUpgrade(module, moduleTemplate)
			Expect(decision).To(Equal(mod.UpgradePending))
			Expect(decision.ShouldApply()).To(BeFalse())
		})
	})

	Context("Deletion Cleanup", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should remove finalizer when Module is deleted (even without a deployed release)", func() {
			nsName := types.NamespacedName{Name: moduleName}
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})

			fetchResource(module, moduleName, "")
			Expect(ctrlutil.ContainsFinalizer(module, mod.ModuleFinalizer)).To(BeTrue())

			Expect(k8sClient.Delete(ctx, module)).To(Succeed())

			Eventually(func() bool {
				var m addonsv1alpha1.Module
				if err := k8sClient.Get(ctx, nsName, &m); err != nil {
					return false
				}
				return !m.DeletionTimestamp.IsZero()
			}, timeout, interval).Should(BeTrue())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var m addonsv1alpha1.Module
				err := k8sClient.Get(ctx, nsName, &m)
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Domain Helpers", func() {
		It("should resolve target namespace correctly", func() {
			mt := &addonsv1alpha1.ModuleTemplate{
				Spec: addonsv1alpha1.ModuleTemplateSpec{Namespace: "template-ns"},
			}

			m := &addonsv1alpha1.Module{}
			Expect(mod.TargetNamespace(m, mt)).To(Equal("template-ns"))

			overrideNS := "module-ns"
			m.Spec.Namespace = &overrideNS
			Expect(mod.TargetNamespace(m, mt)).To(Equal("module-ns"))
		})

		It("should compute upgrade decisions correctly", func() {
			mt := &addonsv1alpha1.ModuleTemplate{}
			mt.Generation = 1

			m := &addonsv1alpha1.Module{}

			By("Initial install")
			Expect(mod.CheckUpgrade(m, mt)).To(Equal(mod.UpgradeInitialInstall))

			By("Not needed when generation matches")
			m.Status.AppliedTemplateGeneration = 1
			Expect(mod.CheckUpgrade(m, mt)).To(Equal(mod.UpgradeNotNeeded))

			By("Auto-approve when no approval configured")
			mt.Generation = 2
			Expect(mod.CheckUpgrade(m, mt)).To(Equal(mod.UpgradeApproved))

			By("Pending when approval configured but not matching")
			approvedGen := int64(1)
			m.Spec.ApprovedTemplateGeneration = &approvedGen
			Expect(mod.CheckUpgrade(m, mt)).To(Equal(mod.UpgradePending))

			By("Approved when approval matches new generation")
			approvedGen = 2
			Expect(mod.CheckUpgrade(m, mt)).To(Equal(mod.UpgradeApproved))
		})
	})

	Context("MapModuleTemplateToModules", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmChartTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should map template changes to referencing modules", func() {
			requests := reconciler.mapModuleTemplateToModules(ctx, moduleTemplate)
			var found bool
			for _, r := range requests {
				if r.Name == moduleName {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		})
	})
})
