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
	"context"
	"encoding/json"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	"github.com/otterscale/module-operator/internal/labels"
	mod "github.com/otterscale/module-operator/internal/module"
)

var _ = Describe("Module Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
		version  = "test-v1"
	)

	var (
		ctx            context.Context
		reconciler     *ModuleReconciler
		module         *modulev1alpha1.Module
		moduleTemplate *modulev1alpha1.ModuleTemplate
		moduleName     string
		templateName   string
		targetNS       string
	)

	// --- Helpers ---

	mustMarshalRaw := func(v any) *runtime.RawExtension {
		raw, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		return &runtime.RawExtension{Raw: raw}
	}

	makeHelmReleaseTemplate := func(name, namespace string) *modulev1alpha1.ModuleTemplate {
		return &modulev1alpha1.ModuleTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: modulev1alpha1.ModuleTemplateSpec{
				Description: "Test HelmRelease module template",
				Namespace:   namespace,
				HelmRelease: mustMarshalRaw(helmv2.HelmReleaseSpec{
					Interval: metav1.Duration{Duration: 10 * time.Minute},
					Chart: &helmv2.HelmChartTemplate{
						Spec: helmv2.HelmChartTemplateSpec{
							Chart: "test-chart",
							SourceRef: helmv2.CrossNamespaceObjectReference{
								Kind: "HelmRepository",
								Name: "test-repo",
							},
						},
					},
				}),
			},
		}
	}

	makeKustomizationTemplate := func(name, namespace string) *modulev1alpha1.ModuleTemplate {
		return &modulev1alpha1.ModuleTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: modulev1alpha1.ModuleTemplateSpec{
				Description: "Test Kustomization module template",
				Namespace:   namespace,
				Kustomization: mustMarshalRaw(kustomizev1.KustomizationSpec{
					Interval: metav1.Duration{Duration: 10 * time.Minute},
					Prune:    true,
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "test-repo",
					},
					Path: "./deploy",
				}),
			},
		}
	}

	makeModule := func(name, templateRef string, mods ...func(*modulev1alpha1.Module)) *modulev1alpha1.Module {
		m := &modulev1alpha1.Module{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: modulev1alpha1.ModuleSpec{
				TemplateRef: templateRef,
			},
		}
		for _, fn := range mods {
			fn(m)
		}
		return m
	}

	ptrInt64 := func(v int64) *int64 { return &v }

	executeReconcile := func() {
		nsName := types.NamespacedName{Name: moduleName}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
	}

	fetchResource := func(obj client.Object, name, namespace string) {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, key, obj)
		}, timeout, interval).Should(Succeed())
	}

	// --- Lifecycle ---

	BeforeEach(func() {
		ctx = context.Background()
		moduleName = "mod-" + string(uuid.NewUUID())[:8]
		templateName = "tmpl-" + string(uuid.NewUUID())[:8]
		targetNS = "ns-" + string(uuid.NewUUID())[:8]
		module = nil
		moduleTemplate = nil
		reconciler = &ModuleReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Version:  version,
			Recorder: events.NewFakeRecorder(100),
		}

		// Pre-create the target namespace (FluxCD resources are namespace-scoped)
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
		// Clean up Module (remove finalizer first if present)
		var m modulev1alpha1.Module
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: moduleName}, &m); err == nil {
			if ctrlutil.ContainsFinalizer(&m, mod.ModuleFinalizer) {
				patch := client.MergeFrom(m.DeepCopy())
				ctrlutil.RemoveFinalizer(&m, mod.ModuleFinalizer)
				_ = k8sClient.Patch(ctx, &m, patch)
			}
			_ = k8sClient.Delete(ctx, &m)
		}

		// Clean up ModuleTemplate
		if moduleTemplate != nil {
			_ = k8sClient.Delete(ctx, moduleTemplate)
		}

		// Clean up HelmRelease if exists
		var hr helmv2.HelmRelease
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: moduleName, Namespace: targetNS}, &hr); err == nil {
			_ = k8sClient.Delete(ctx, &hr)
		}

		// Clean up Kustomization if exists
		var ks kustomizev1.Kustomization
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: moduleName, Namespace: targetNS}, &ks); err == nil {
			_ = k8sClient.Delete(ctx, &ks)
		}
	})

	// --- Tests ---

	Context("HelmRelease Reconciliation", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should create a HelmRelease and update Module status", func() {
			executeReconcile()

			By("Verifying the HelmRelease is created")
			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("test-chart"))

			By("Verifying labels on the HelmRelease")
			Expect(hr.Labels).To(HaveKeyWithValue(labels.Name, moduleName))
			Expect(hr.Labels).To(HaveKeyWithValue(labels.ManagedBy, "module-operator"))
			Expect(hr.Labels).To(HaveKeyWithValue(labels.Component, "module"))
			Expect(hr.Labels).To(HaveKeyWithValue(labels.Version, version))
			Expect(hr.Labels).To(HaveKeyWithValue(mod.LabelModuleTemplate, templateName))

			By("Verifying OwnerReference is set")
			Expect(hr.OwnerReferences).To(HaveLen(1))
			Expect(hr.OwnerReferences[0].Name).To(Equal(moduleName))

			By("Verifying Module status")
			fetchResource(module, moduleName, "")
			Expect(module.Status.ObservedGeneration).To(Equal(module.Generation))
			Expect(module.Status.AppliedTemplateGeneration).To(Equal(moduleTemplate.Generation))
			Expect(module.Status.AvailableTemplateGeneration).To(Equal(moduleTemplate.Generation))
			Expect(module.Status.HelmReleaseRef).NotTo(BeNil())
			Expect(module.Status.HelmReleaseRef.Name).To(Equal(moduleName))
			Expect(module.Status.HelmReleaseRef.Namespace).To(Equal(targetNS))
			Expect(module.Status.KustomizationRef).To(BeNil())

			By("Verifying Ready condition reflects HelmRelease pending state")
			readyCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			// HelmRelease just created, no Ready condition yet → Unknown/Pending
			Expect(readyCond.Reason).To(Equal("HelmReleasePending"))
		})

		It("should add finalizer to the Module", func() {
			executeReconcile()

			fetchResource(module, moduleName, "")
			Expect(ctrlutil.ContainsFinalizer(module, mod.ModuleFinalizer)).To(BeTrue())
		})

		It("should be idempotent on repeated reconciliation", func() {
			executeReconcile()
			executeReconcile()

			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("test-chart"))
		})
	})

	Context("Kustomization Reconciliation", func() {
		BeforeEach(func() {
			moduleTemplate = makeKustomizationTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should create a Kustomization and update Module status", func() {
			executeReconcile()

			By("Verifying the Kustomization is created")
			var ks kustomizev1.Kustomization
			fetchResource(&ks, moduleName, targetNS)
			Expect(ks.Spec.Path).To(Equal("./deploy"))
			Expect(ks.Spec.Prune).To(BeTrue())
			Expect(ks.Spec.SourceRef.Name).To(Equal("test-repo"))

			By("Verifying labels on the Kustomization")
			Expect(ks.Labels).To(HaveKeyWithValue(labels.Name, moduleName))
			Expect(ks.Labels).To(HaveKeyWithValue(labels.ManagedBy, "module-operator"))

			By("Verifying Module status")
			fetchResource(module, moduleName, "")
			Expect(module.Status.KustomizationRef).NotTo(BeNil())
			Expect(module.Status.KustomizationRef.Name).To(Equal(moduleName))
			Expect(module.Status.KustomizationRef.Namespace).To(Equal(targetNS))
			Expect(module.Status.HelmReleaseRef).To(BeNil())
		})
	})

	Context("Template Not Found", func() {
		BeforeEach(func() {
			moduleTemplate = nil // Do NOT create a template
			module = makeModule(moduleName, "non-existent-template")
		})

		It("should set Ready=False with TemplateNotFound and not return error", func() {
			By("Reconciling - TemplateNotFound is permanent: should NOT return error (no requeue)")
			nsName := types.NamespacedName{Name: moduleName}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the status condition")
			fetchResource(module, moduleName, "")
			readyCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("TemplateNotFound"))
		})
	})

	Context("Namespace Override", func() {
		var overrideNS string

		BeforeEach(func() {
			overrideNS = "override-" + string(uuid.NewUUID())[:8]

			// Create the override namespace
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: overrideNS},
			})).To(Succeed())

			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName, func(m *modulev1alpha1.Module) {
				m.Spec.Namespace = &overrideNS
			})
		})

		It("should create the HelmRelease in the overridden namespace", func() {
			executeReconcile()

			By("Verifying HelmRelease is in the override namespace, not the template default")
			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, overrideNS)
			Expect(hr.Namespace).To(Equal(overrideNS))

			By("Verifying status ref points to the override namespace")
			fetchResource(module, moduleName, "")
			Expect(module.Status.HelmReleaseRef).NotTo(BeNil())
			Expect(module.Status.HelmReleaseRef.Namespace).To(Equal(overrideNS))

			By("Verifying no HelmRelease in the template's default namespace")
			var hrDefault helmv2.HelmRelease
			err := k8sClient.Get(ctx, types.NamespacedName{Name: moduleName, Namespace: targetNS}, &hrDefault)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("Values Override", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			overrideValues := map[string]any{
				"replicaCount": 5,
				"image":        map[string]any{"tag": "custom"},
			}
			valuesJSON, err := json.Marshal(overrideValues)
			Expect(err).NotTo(HaveOccurred())

			module = makeModule(moduleName, templateName, func(m *modulev1alpha1.Module) {
				m.Spec.Values = &runtime.RawExtension{Raw: valuesJSON}
			})
		})

		It("should apply values override to the HelmRelease", func() {
			executeReconcile()

			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)

			By("Verifying values are overridden")
			Expect(hr.Spec.Values).NotTo(BeNil())

			var actual map[string]any
			Expect(json.Unmarshal(hr.Spec.Values.Raw, &actual)).To(Succeed())
			Expect(actual["replicaCount"]).To(BeEquivalentTo(5))
			Expect(actual["image"]).To(HaveKeyWithValue("tag", "custom"))
		})
	})

	Context("Deletion Cleanup", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should delete the HelmRelease when the Module is deleted", func() {
			By("First reconcile to create resources")
			executeReconcile()

			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)

			By("Deleting the Module")
			fetchResource(module, moduleName, "")
			Expect(k8sClient.Delete(ctx, module)).To(Succeed())

			By("Reconciling deletion")
			nsName := types.NamespacedName{Name: moduleName}
			// Refetch after delete to get DeletionTimestamp
			Eventually(func() bool {
				var m modulev1alpha1.Module
				if err := k8sClient.Get(ctx, nsName, &m); err != nil {
					return false
				}
				return !m.DeletionTimestamp.IsZero()
			}, timeout, interval).Should(BeTrue())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the HelmRelease is deleted")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: moduleName, Namespace: targetNS}, &hr))
			}, timeout, interval).Should(BeTrue())

			By("Verifying the Module finalizer is removed and Module is gone")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, nsName, &modulev1alpha1.Module{}))
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Template Generation Tracking", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should track the template generation in Module status", func() {
			executeReconcile()

			fetchResource(module, moduleName, "")
			initialAppliedGen := module.Status.AppliedTemplateGeneration

			By("Updating the ModuleTemplate")
			fetchResource(moduleTemplate, templateName, "")
			moduleTemplate.Spec.Description = "Updated description"
			Expect(k8sClient.Update(ctx, moduleTemplate)).To(Succeed())

			By("Re-reconciling (no approval configured → auto-approve)")
			executeReconcile()

			By("Verifying applied template generation is updated")
			fetchResource(module, moduleName, "")
			Expect(module.Status.AppliedTemplateGeneration).To(BeNumerically(">", initialAppliedGen))
			Expect(module.Status.AvailableTemplateGeneration).To(Equal(module.Status.AppliedTemplateGeneration))
		})
	})

	Context("Upgrade Pending", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should not update FluxCD resources when upgrade is not approved", func() {
			By("Initial install (auto-applied)")
			executeReconcile()

			fetchResource(module, moduleName, "")
			initialGen := module.Status.AppliedTemplateGeneration

			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("test-chart"))

			By("Locking the Module to require approval")
			fetchResource(module, moduleName, "")
			patch := client.MergeFrom(module.DeepCopy())
			module.Spec.ApprovedTemplateGeneration = ptrInt64(initialGen)
			Expect(k8sClient.Patch(ctx, module, patch)).To(Succeed())

			By("Updating the ModuleTemplate (simulates a new app version)")
			fetchResource(moduleTemplate, templateName, "")
			moduleTemplate.Spec.HelmRelease = mustMarshalRaw(helmv2.HelmReleaseSpec{
				Interval: metav1.Duration{Duration: 10 * time.Minute},
				Chart: &helmv2.HelmChartTemplate{
					Spec: helmv2.HelmChartTemplateSpec{
						Chart: "test-chart-v2",
						SourceRef: helmv2.CrossNamespaceObjectReference{
							Kind: "HelmRepository",
							Name: "test-repo",
						},
					},
				},
			})
			Expect(k8sClient.Update(ctx, moduleTemplate)).To(Succeed())

			By("Reconciling — upgrade should be pending, not applied")
			executeReconcile()

			By("Verifying HelmRelease still has the old chart")
			fetchResource(&hr, moduleName, targetNS)
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("test-chart"))

			By("Verifying UpgradeAvailable condition is set")
			fetchResource(module, moduleName, "")
			upgradeCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeUpgradeAvailable)
			Expect(upgradeCond).NotTo(BeNil())
			Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(upgradeCond.Reason).To(Equal("UpgradePending"))

			By("Verifying AppliedTemplateGeneration is unchanged")
			Expect(module.Status.AppliedTemplateGeneration).To(Equal(initialGen))

			By("Verifying AvailableTemplateGeneration reflects the new template")
			Expect(module.Status.AvailableTemplateGeneration).To(BeNumerically(">", initialGen))
		})
	})

	Context("Upgrade Approved", func() {
		BeforeEach(func() {
			moduleTemplate = makeHelmReleaseTemplate(templateName, targetNS)
			module = makeModule(moduleName, templateName)
		})

		It("should apply template changes once approved", func() {
			By("Initial install")
			executeReconcile()

			fetchResource(module, moduleName, "")
			initialGen := module.Status.AppliedTemplateGeneration

			By("Locking the Module to require approval")
			fetchResource(module, moduleName, "")
			patch := client.MergeFrom(module.DeepCopy())
			module.Spec.ApprovedTemplateGeneration = ptrInt64(initialGen)
			Expect(k8sClient.Patch(ctx, module, patch)).To(Succeed())

			By("Updating the ModuleTemplate")
			fetchResource(moduleTemplate, templateName, "")
			moduleTemplate.Spec.HelmRelease = mustMarshalRaw(helmv2.HelmReleaseSpec{
				Interval: metav1.Duration{Duration: 10 * time.Minute},
				Chart: &helmv2.HelmChartTemplate{
					Spec: helmv2.HelmChartTemplateSpec{
						Chart: "test-chart-v2",
						SourceRef: helmv2.CrossNamespaceObjectReference{
							Kind: "HelmRepository",
							Name: "test-repo",
						},
					},
				},
			})
			Expect(k8sClient.Update(ctx, moduleTemplate)).To(Succeed())

			By("Reconciling — should be pending")
			executeReconcile()

			fetchResource(module, moduleName, "")
			newAvailableGen := module.Status.AvailableTemplateGeneration
			Expect(newAvailableGen).To(BeNumerically(">", initialGen))

			By("Approving the upgrade")
			fetchResource(module, moduleName, "")
			patch = client.MergeFrom(module.DeepCopy())
			module.Spec.ApprovedTemplateGeneration = ptrInt64(newAvailableGen)
			Expect(k8sClient.Patch(ctx, module, patch)).To(Succeed())

			By("Reconciling — should now apply the upgrade")
			executeReconcile()

			By("Verifying HelmRelease has the new chart")
			var hr helmv2.HelmRelease
			fetchResource(&hr, moduleName, targetNS)
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("test-chart-v2"))

			By("Verifying UpgradeAvailable condition is removed")
			fetchResource(module, moduleName, "")
			upgradeCond := meta.FindStatusCondition(module.Status.Conditions, mod.ConditionTypeUpgradeAvailable)
			Expect(upgradeCond).To(BeNil())

			By("Verifying AppliedTemplateGeneration is updated")
			Expect(module.Status.AppliedTemplateGeneration).To(Equal(newAvailableGen))
			Expect(module.Status.AvailableTemplateGeneration).To(Equal(newAvailableGen))
		})
	})

	Context("Domain Helpers", func() {
		It("should generate correct labels", func() {
			moduleLabels := mod.LabelsForModule("my-module", "my-template", "v1.0.0")
			Expect(moduleLabels).To(HaveKeyWithValue(labels.Name, "my-module"))
			Expect(moduleLabels).To(HaveKeyWithValue(labels.ManagedBy, "module-operator"))
			Expect(moduleLabels).To(HaveKeyWithValue(labels.PartOf, "otterscale-system"))
			Expect(moduleLabels).To(HaveKeyWithValue(labels.Component, "module"))
			Expect(moduleLabels).To(HaveKeyWithValue(labels.Version, "v1.0.0"))
			Expect(moduleLabels).To(HaveKeyWithValue(mod.LabelModuleTemplate, "my-template"))
		})

		It("should resolve target namespace correctly", func() {
			mt := &modulev1alpha1.ModuleTemplate{
				Spec: modulev1alpha1.ModuleTemplateSpec{Namespace: "template-ns"},
			}

			By("Using template default when Module has no override")
			m := &modulev1alpha1.Module{}
			Expect(mod.TargetNamespace(m, mt)).To(Equal("template-ns"))

			By("Using Module override when specified")
			overrideNS := "module-ns"
			m.Spec.Namespace = &overrideNS
			Expect(mod.TargetNamespace(m, mt)).To(Equal("module-ns"))
		})
	})
})
