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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/client-go/tools/events"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
	"github.com/otterscale/module-operator/internal/labels"
	mod "github.com/otterscale/module-operator/internal/module"
)

// ModuleReconciler reconciles a Module object.
// It ensures that the FluxCD HelmRelease or Kustomization matches the desired state
// derived from the referenced ModuleTemplate.
//
// The controller is intentionally kept thin: it orchestrates the reconciliation flow,
// while the actual resource synchronization logic resides in internal/core/module/.
type ModuleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Version  string
	Recorder events.EventRecorder
}

// RBAC Permissions required by the controller:
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules/finalizers,verbs=update
// +kubebuilder:rbac:groups=module.otterscale.io,resources=moduletemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main loop for the Module controller.
// It implements the level-triggered reconciliation logic:
// Fetch -> Finalizer -> Fetch Template -> Check Upgrade -> Sync FluxCD Resource -> Status Update.
//
<<<<<<< HEAD
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
=======
// When a ModuleTemplate changes, the controller evaluates an upgrade decision.
// If the Module has an explicit ApprovedTemplateGeneration, template changes are
// gated until the user approves the new generation. Otherwise, changes are auto-applied.
//
// Deletion is handled via Finalizer to ensure FluxCD resources are properly cleaned up
// (allowing Flux to run its uninstall logic) before the Module is removed.
>>>>>>> tmp-original-31-03-26-01-28
func (r *ModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.Name)
	ctx = log.IntoContext(ctx, logger)

	// 1. Fetch the Module instance
	var m modulev1alpha1.Module
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Handle deletion with Finalizer
	if !m.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &m)
	}

	// 3. Ensure Finalizer is present
	if !ctrlutil.ContainsFinalizer(&m, mod.ModuleFinalizer) {
		patch := client.MergeFrom(m.DeepCopy())
		ctrlutil.AddFinalizer(&m, mod.ModuleFinalizer)
		if err := r.Patch(ctx, &m, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Fetch the referenced ModuleTemplate
	mt, err := r.fetchModuleTemplate(ctx, m.Spec.TemplateRef)
	if err != nil {
		return r.handleReconcileError(ctx, &m, err)
	}

	// 5. Evaluate upgrade decision
	decision := mod.CheckUpgrade(&m, mt)

	// 6. Apply template only when the decision permits it
	if decision.ShouldApply() {
		if err := r.reconcileResources(ctx, &m, mt); err != nil {
			return r.handleReconcileError(ctx, &m, err)
		}
	} else {
		logger.Info("Upgrade pending approval",
			"available", mt.Generation,
			"applied", m.Status.AppliedTemplateGeneration)
	}

	// 7. Update Status (upgrade-aware)
	if err := r.updateStatus(ctx, &m, mt, decision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// fetchModuleTemplate retrieves the ModuleTemplate referenced by the Module.
// Returns a TemplateNotFoundError (permanent) if the template does not exist.
func (r *ModuleReconciler) fetchModuleTemplate(ctx context.Context, name string) (*modulev1alpha1.ModuleTemplate, error) {
	var mt modulev1alpha1.ModuleTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &mt); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &mod.TemplateNotFoundError{Name: name}
		}
		return nil, err
	}
	return &mt, nil
}

// reconcileResources dispatches to the appropriate domain sync function
// based on the template type (HelmRelease or Kustomization).
// If the template type was switched (e.g. from HelmRelease to Kustomization),
// the stale FluxCD resource of the previous type is cleaned up.
func (r *ModuleReconciler) reconcileResources(ctx context.Context, m *modulev1alpha1.Module, mt *modulev1alpha1.ModuleTemplate) error {
	switch {
	case mt.Spec.HelmRelease != nil:
		if err := mod.ReconcileHelmRelease(ctx, r.Client, r.Scheme, m, mt, r.Version); err != nil {
			return err
		}
		if ref := m.Status.KustomizationRef; ref != nil && ref.Namespace != "" {
			if err := mod.DeleteKustomization(ctx, r.Client, m, ref.Namespace); err != nil {
				return err
			}
		}
		return nil
	case mt.Spec.Kustomization != nil:
		if err := mod.ReconcileKustomization(ctx, r.Client, r.Scheme, m, mt, r.Version); err != nil {
			return err
		}
		if ref := m.Status.HelmReleaseRef; ref != nil && ref.Namespace != "" {
			if err := mod.DeleteHelmRelease(ctx, r.Client, m, ref.Namespace); err != nil {
				return err
			}
		}
		return nil
	default:
		return &mod.TemplateInvalidError{
			Name:    mt.Name,
			Message: "neither helmRelease nor kustomization is defined",
		}
	}
}

// reconcileDelete handles the deletion flow:
// 1. Delete the FluxCD resource
// 2. Remove the Finalizer
func (r *ModuleReconciler) reconcileDelete(ctx context.Context, m *modulev1alpha1.Module) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if ctrlutil.ContainsFinalizer(m, mod.ModuleFinalizer) {
		logger.Info("Cleaning up FluxCD resources before Module deletion")

		// Attempt to resolve the namespace for cleanup.
		// We try to fetch the template; if it's gone, fall back to the Module's namespace override
		// or the status refs to determine where the FluxCD resource lives.
		namespace := r.resolveCleanupNamespace(ctx, m)

		if namespace != "" {
			// Delete based on what type of resource was created (check status refs)
			if m.Status.HelmReleaseRef != nil {
				if err := mod.DeleteHelmRelease(ctx, r.Client, m, namespace); err != nil {
					return ctrl.Result{}, err
				}
			}
			if m.Status.KustomizationRef != nil {
				if err := mod.DeleteKustomization(ctx, r.Client, m, namespace); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		// Remove finalizer using Patch to avoid ResourceVersion conflicts
		// under high concurrency (consistent with how we add the finalizer).
		patch := client.MergeFrom(m.DeepCopy())
		ctrlutil.RemoveFinalizer(m, mod.ModuleFinalizer)
		if err := r.Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// resolveCleanupNamespace determines the namespace of FluxCD resources for cleanup.
// Priority: Status refs > Module spec override > ModuleTemplate default.
func (r *ModuleReconciler) resolveCleanupNamespace(ctx context.Context, m *modulev1alpha1.Module) string {
	// First, try to get it from status refs (most reliable, reflects actual state)
	if m.Status.HelmReleaseRef != nil && m.Status.HelmReleaseRef.Namespace != "" {
		return m.Status.HelmReleaseRef.Namespace
	}
	if m.Status.KustomizationRef != nil && m.Status.KustomizationRef.Namespace != "" {
		return m.Status.KustomizationRef.Namespace
	}

	// Fall back to Module spec
	if m.Spec.Namespace != nil {
		return *m.Spec.Namespace
	}

	// Last resort: try to fetch the template
	mt, err := r.fetchModuleTemplate(ctx, m.Spec.TemplateRef)
	if err != nil {
		return ""
	}
	return mt.Spec.Namespace
}

// handleReconcileError categorizes errors and updates status accordingly.
// Permanent errors (TemplateNotFound, TemplateInvalid) do NOT requeue unless
// the status patch itself fails. Transient errors are returned to
// controller-runtime for exponential backoff retry.
func (r *ModuleReconciler) handleReconcileError(ctx context.Context, m *modulev1alpha1.Module, err error) (ctrl.Result, error) {
	var tnf *mod.TemplateNotFoundError
	var tie *mod.TemplateInvalidError

	switch {
	case errors.As(err, &tnf):
		if patchErr := r.setReadyConditionFalse(ctx, m, "TemplateNotFound", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "TemplateNotFound", "Reconcile", err.Error())
		return ctrl.Result{}, nil

	case errors.As(err, &tie):
		if patchErr := r.setReadyConditionFalse(ctx, m, "TemplateInvalid", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "TemplateInvalid", "Reconcile", err.Error())
		return ctrl.Result{}, nil

	default:
		if patchErr := r.setReadyConditionFalse(ctx, m, "ReconcileError", err.Error()); patchErr != nil {
			log.FromContext(ctx).Error(patchErr, "Failed to patch status while handling reconcile error")
		}
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", err.Error())
		return ctrl.Result{}, err
	}
}

// setReadyConditionFalse updates the Ready condition to False via status patch.
// Returns error so callers can decide whether to requeue on patch failure.
func (r *ModuleReconciler) setReadyConditionFalse(ctx context.Context, m *modulev1alpha1.Module, reason, message string) error {
	patch := client.MergeFrom(m.DeepCopy())
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               mod.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: m.Generation,
	})
	m.Status.ObservedGeneration = m.Generation

	return r.Status().Patch(ctx, m, patch)
}

// updateStatus calculates the status based on the current observed state, the upgrade
// decision, and patches the resource. It always records the available template generation.
// Resource refs and applied generation are only updated when the template was actually applied.
func (r *ModuleReconciler) updateStatus(ctx context.Context, m *modulev1alpha1.Module, mt *modulev1alpha1.ModuleTemplate, decision mod.UpgradeDecision) error {
	newStatus := m.Status.DeepCopy()
	newStatus.ObservedGeneration = m.Generation
	newStatus.AvailableTemplateGeneration = mt.Generation

	var observeNS string

	switch decision {
	case mod.UpgradeInitialInstall, mod.UpgradeApproved:
		newStatus.AppliedTemplateGeneration = mt.Generation
		targetNS := mod.TargetNamespace(m, mt)
		observeNS = targetNS
		r.setResourceRefs(newStatus, m.Name, targetNS, mt)

	case mod.UpgradeNotNeeded:
		targetNS := mod.TargetNamespace(m, mt)
		observeNS = targetNS
		r.setResourceRefs(newStatus, m.Name, targetNS, mt)

	case mod.UpgradePending:
		// Don't update AppliedTemplateGeneration or resource refs.
		// Observe the currently deployed FluxCD resource via status refs.
		observeNS = r.namespaceFromStatusRefs(m)
	}

	newStatus.Namespace = observeNS

	// Observe the FluxCD resource status (uses the currently deployed namespace).
	// During UpgradePending the template may have changed resource type, so we
	// determine which FluxCD resource to observe from the actual status refs
	// rather than the (possibly stale) template.
	if observeNS != "" {
		var readyStatus metav1.ConditionStatus
		var readyReason, readyMessage string

		if decision == mod.UpgradePending {
			switch {
			case m.Status.HelmReleaseRef != nil:
				readyStatus, readyReason, readyMessage = r.observeHelmReleaseStatus(ctx, m.Name, observeNS)
			case m.Status.KustomizationRef != nil:
				readyStatus, readyReason, readyMessage = r.observeKustomizationStatus(ctx, m.Name, observeNS)
			default:
				readyStatus, readyReason, readyMessage = metav1.ConditionUnknown, "ResourceTypeUnknown", "no deployed resource reference found"
			}
		} else {
			readyStatus, readyReason, readyMessage = r.observeFluxResourceStatus(ctx, m, mt, observeNS)
		}

		meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
			Type:               mod.ConditionTypeReady,
			Status:             readyStatus,
			Reason:             readyReason,
			Message:            readyMessage,
			ObservedGeneration: m.Generation,
		})
	}

	// Set or clear the UpgradeAvailable condition
	if decision == mod.UpgradePending {
		meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
			Type:               mod.ConditionTypeUpgradeAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "UpgradePending",
			Message:            fmt.Sprintf("Template generation %d available (applied: %d)", mt.Generation, newStatus.AppliedTemplateGeneration),
			ObservedGeneration: m.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&newStatus.Conditions, mod.ConditionTypeUpgradeAvailable)
	}

	// Sort conditions by type for stable ordering
	slices.SortFunc(newStatus.Conditions, func(a, b metav1.Condition) int {
		return cmp.Compare(a.Type, b.Type)
	})

	// Only patch if status has changed to reduce API server load
	if !equality.Semantic.DeepEqual(m.Status, *newStatus) {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status = *newStatus
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Module status updated")
		r.Recorder.Eventf(m, nil, corev1.EventTypeNormal, "Reconciled", "Reconcile",
			"Module resources reconciled for template %s", m.Spec.TemplateRef)
	}

	return nil
}

// setResourceRefs updates the FluxCD resource references in the status based on template type.
func (r *ModuleReconciler) setResourceRefs(status *modulev1alpha1.ModuleStatus, name, namespace string, mt *modulev1alpha1.ModuleTemplate) {
	switch {
	case mt.Spec.HelmRelease != nil:
		status.HelmReleaseRef = &modulev1alpha1.ResourceReference{
			Name:      name,
			Namespace: namespace,
		}
		status.KustomizationRef = nil
	case mt.Spec.Kustomization != nil:
		status.KustomizationRef = &modulev1alpha1.ResourceReference{
			Name:      name,
			Namespace: namespace,
		}
		status.HelmReleaseRef = nil
	}
}

// namespaceFromStatusRefs returns the namespace of the currently deployed FluxCD resource
// from the Module's status refs. Used during UpgradePending to observe the existing resource
// without relying on the (potentially changed) template.
func (r *ModuleReconciler) namespaceFromStatusRefs(m *modulev1alpha1.Module) string {
	if m.Status.HelmReleaseRef != nil {
		return m.Status.HelmReleaseRef.Namespace
	}
	if m.Status.KustomizationRef != nil {
		return m.Status.KustomizationRef.Namespace
	}
	return ""
}

// observeFluxResourceStatus reads the Ready condition from the FluxCD resource
// and translates it into the Module's status.
func (r *ModuleReconciler) observeFluxResourceStatus(
	ctx context.Context,
	m *modulev1alpha1.Module,
	mt *modulev1alpha1.ModuleTemplate,
	namespace string,
) (metav1.ConditionStatus, string, string) {
	switch {
	case mt.Spec.HelmRelease != nil:
		return r.observeHelmReleaseStatus(ctx, m.Name, namespace)
	case mt.Spec.Kustomization != nil:
		return r.observeKustomizationStatus(ctx, m.Name, namespace)
	default:
		return metav1.ConditionFalse, "TemplateInvalid", "no flux resource type defined"
	}
}

// observeHelmReleaseStatus reads the HelmRelease Ready condition.
func (r *ModuleReconciler) observeHelmReleaseStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
	var hr helmv2.HelmRelease
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &hr); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "HelmReleaseNotFound", "waiting for HelmRelease to be created"
		}
		return metav1.ConditionUnknown, "HelmReleaseGetError", err.Error()
	}

	readyCond := meta.FindStatusCondition(hr.Status.Conditions, "Ready")
	if readyCond == nil {
		return metav1.ConditionUnknown, "HelmReleasePending", "HelmRelease has no Ready condition yet"
	}

	return readyCond.Status, "HelmRelease" + readyCond.Reason, readyCond.Message
}

// observeKustomizationStatus reads the Kustomization Ready condition.
func (r *ModuleReconciler) observeKustomizationStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &ks); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "KustomizationNotFound", "waiting for Kustomization to be created"
		}
		return metav1.ConditionUnknown, "KustomizationGetError", err.Error()
	}

	readyCond := meta.FindStatusCondition(ks.Status.Conditions, "Ready")
	if readyCond == nil {
		return metav1.ConditionUnknown, "KustomizationPending", "Kustomization has no Ready condition yet"
	}

	return readyCond.Status, "Kustomization" + readyCond.Reason, readyCond.Message
}

// indexFieldTemplateRef is the field index key for Module.spec.templateRef,
// used for server-side filtering in mapModuleTemplateToModules.
const indexFieldTemplateRef = ".spec.templateRef"

// SetupWithManager registers the controller with the Manager and defines watches.
//
// Watch configuration:
//   - Module: with GenerationChangedPredicate to skip status-only updates
//   - ModuleTemplate: when changed, all Modules referencing it are re-enqueued
//   - FluxCD HelmRelease/Kustomization: status changes trigger Module re-reconciliation
//     via label-based mapping (the operator labels all managed FluxCD resources)
func (r *ModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&modulev1alpha1.Module{},
		indexFieldTemplateRef,
		func(obj client.Object) []string {
			m, ok := obj.(*modulev1alpha1.Module)
			if !ok {
				return nil
			}
			return []string{m.Spec.TemplateRef}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&modulev1alpha1.Module{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// Watch ModuleTemplate changes → re-enqueue all Modules referencing the changed template
		Watches(
			&modulev1alpha1.ModuleTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.mapModuleTemplateToModules),
		).
		// Watch owned FluxCD HelmRelease for status changes
		Watches(
			&helmv2.HelmRelease{},
			handler.EnqueueRequestsFromMapFunc(r.mapFluxResourceToModule),
		).
		// Watch owned FluxCD Kustomization for status changes
		Watches(
			&kustomizev1.Kustomization{},
			handler.EnqueueRequestsFromMapFunc(r.mapFluxResourceToModule),
		).
		Named("module").
		Complete(r)
}

// mapModuleTemplateToModules enqueues all Modules that reference the changed ModuleTemplate.
// Uses the field index on .spec.templateRef for efficient server-side filtering.
func (r *ModuleReconciler) mapModuleTemplateToModules(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("template-watch")
	templateName := obj.GetName()

	var modules modulev1alpha1.ModuleList
	if err := r.List(ctx, &modules, client.MatchingFields{indexFieldTemplateRef: templateName}); err != nil {
		logger.Error(err, "Failed to list Modules for ModuleTemplate change re-enqueue")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(modules.Items))
	for _, m := range modules.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: m.Name},
		})
	}

	if len(requests) > 0 {
		logger.Info("ModuleTemplate changed, re-enqueuing referencing Modules",
			"template", templateName, "count", len(requests))
	}
	return requests
}

// mapFluxResourceToModule maps a FluxCD resource back to its owning Module
// using the instance label set by the operator.
func (r *ModuleReconciler) mapFluxResourceToModule(_ context.Context, obj client.Object) []reconcile.Request {
	objLabels := obj.GetLabels()
	if objLabels == nil {
		return nil
	}

	// Only handle resources managed by us
	if objLabels[labels.ManagedBy] != "module-operator" || objLabels[labels.Component] != "module" {
		return nil
	}

	moduleName, ok := objLabels[labels.Name]
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: moduleName}},
	}
}
