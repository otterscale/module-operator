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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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
	mod "github.com/otterscale/module-operator/internal/module"
)

// ModuleReconciler reconciles a Module object.
// It manages the lifecycle of the underlying Helm release or Kustomize
// manifests directly, without depending on external controllers.
//
// The controller is intentionally kept thin: it orchestrates the reconciliation flow,
// while the actual resource synchronization logic resides in internal/module/.
type ModuleReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
	Version    string
	Recorder   events.EventRecorder
}

// RBAC Permissions required by the controller:
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=module.otterscale.io,resources=modules/finalizers,verbs=update
// +kubebuilder:rbac:groups=module.otterscale.io,resources=moduleclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main loop for the Module controller.
// It implements the level-triggered reconciliation logic:
// Fetch -> Finalizer -> Fetch Class -> Check Upgrade -> Reconcile Resources -> Status Update.
func (r *ModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.Name)
	ctx = log.IntoContext(ctx, logger)

	var m modulev1alpha1.Module
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !m.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &m)
	}

	if !ctrlutil.ContainsFinalizer(&m, mod.ModuleFinalizer) {
		patch := client.MergeFrom(m.DeepCopy())
		ctrlutil.AddFinalizer(&m, mod.ModuleFinalizer)
		if err := r.Patch(ctx, &m, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	mc, err := r.fetchModuleClass(ctx, m.Spec.ModuleClassName)
	if err != nil {
		return r.handleReconcileError(ctx, &m, err)
	}

	decision := mod.CheckUpgrade(&m, mc)

	var requeueAfter ctrl.Result

	if decision.ShouldApply() {
		result, reconcileErr := r.reconcileResources(ctx, &m, mc)
		if reconcileErr != nil {
			return r.handleReconcileError(ctx, &m, reconcileErr)
		}
		if err := r.updateStatus(ctx, &m, mc, decision, result); err != nil {
			return ctrl.Result{}, err
		}
		requeueAfter = r.requeueInterval(mc)
	} else {
		logger.Info("Upgrade pending approval",
			"available", mc.Generation,
			"applied", m.Status.AppliedClassGeneration)
		if err := r.updateStatus(ctx, &m, mc, decision, nil); err != nil {
			return ctrl.Result{}, err
		}
		requeueAfter = r.requeueInterval(mc)
	}

	return requeueAfter, nil
}

// fetchModuleClass retrieves the ModuleClass referenced by the Module.
func (r *ModuleReconciler) fetchModuleClass(ctx context.Context, name string) (*modulev1alpha1.ModuleClass, error) {
	var mc modulev1alpha1.ModuleClass
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &mc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &mod.ClassNotFoundError{Name: name}
		}
		return nil, err
	}
	return &mc, nil
}

// reconcileResult is a union type carrying the outcome of either a Helm
// or Kustomize reconciliation.
type reconcileResult struct {
	Helm      *mod.HelmReconcileResult
	Kustomize *mod.KustomizeReconcileResult
}

// reconcileResources dispatches to the appropriate domain function
// based on the class type (HelmChart or Kustomization).
func (r *ModuleReconciler) reconcileResources(ctx context.Context, m *modulev1alpha1.Module, mc *modulev1alpha1.ModuleClass) (*reconcileResult, error) {
	switch {
	case mc.Spec.HelmChart != nil:
		res, err := mod.ReconcileHelmChart(ctx, r.Client, r.RestConfig, m, mc, r.Version)
		if err != nil {
			return nil, err
		}
		return &reconcileResult{Helm: res}, nil
	case mc.Spec.Kustomization != nil:
		res, err := mod.ReconcileKustomization(ctx, r.Client, r.RestConfig, m, mc, r.Version)
		if err != nil {
			return nil, err
		}
		return &reconcileResult{Kustomize: res}, nil
	default:
		return nil, &mod.ClassInvalidError{
			Name:    mc.Name,
			Message: "neither helmChart nor kustomization is defined",
		}
	}
}

// reconcileDelete handles the deletion flow:
// 1. Clean up managed resources (Helm uninstall or Kustomize prune)
// 2. Remove the Finalizer
func (r *ModuleReconciler) reconcileDelete(ctx context.Context, m *modulev1alpha1.Module) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if ctrlutil.ContainsFinalizer(m, mod.ModuleFinalizer) {
		logger.Info("Cleaning up managed resources before Module deletion")

		if m.Status.HelmRelease != nil {
			namespace := r.resolveCleanupNamespace(ctx, m)
			releaseName := m.Name
			mc, err := r.fetchModuleClass(ctx, m.Spec.ModuleClassName)
			if err == nil && mc.Spec.HelmChart != nil && mc.Spec.HelmChart.ReleaseName != "" {
				releaseName = mc.Spec.HelmChart.ReleaseName
			}
			if namespace != "" {
				if err := mod.UninstallHelmChart(ctx, r.RestConfig, releaseName, namespace); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		if m.Status.Kustomization != nil || len(m.Status.Inventory) > 0 {
			if err := mod.DeleteKustomization(ctx, r.RestConfig, m.Status.Inventory); err != nil {
				return ctrl.Result{}, err
			}
		}

		patch := client.MergeFrom(m.DeepCopy())
		ctrlutil.RemoveFinalizer(m, mod.ModuleFinalizer)
		if err := r.Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// resolveCleanupNamespace determines the namespace of managed resources for cleanup.
func (r *ModuleReconciler) resolveCleanupNamespace(ctx context.Context, m *modulev1alpha1.Module) string {
	if m.Status.Namespace != "" {
		return m.Status.Namespace
	}
	if m.Spec.Namespace != nil {
		return *m.Spec.Namespace
	}
	mc, err := r.fetchModuleClass(ctx, m.Spec.ModuleClassName)
	if err != nil {
		return ""
	}
	return mc.Spec.Namespace
}

// handleReconcileError categorizes errors and updates status accordingly.
// Permanent errors (ClassNotFound, ClassInvalid) do NOT requeue.
// Transient errors are returned to controller-runtime for exponential backoff retry.
func (r *ModuleReconciler) handleReconcileError(ctx context.Context, m *modulev1alpha1.Module, err error) (ctrl.Result, error) {
	var cnf *mod.ClassNotFoundError
	var cie *mod.ClassInvalidError
	var cfe *mod.ChartFetchError
	var sfe *mod.SourceFetchError

	switch {
	case errors.As(err, &cnf):
		r.setReadyConditionFalse(ctx, m, "ClassNotFound", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "ClassNotFound", "Reconcile", err.Error())
		return ctrl.Result{}, nil

	case errors.As(err, &cie):
		r.setReadyConditionFalse(ctx, m, "ClassInvalid", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "ClassInvalid", "Reconcile", err.Error())
		return ctrl.Result{}, nil

	case errors.As(err, &cfe):
		r.setReadyConditionFalse(ctx, m, "ChartFetchError", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "ChartFetchError", "Reconcile", err.Error())
		return ctrl.Result{}, err

	case errors.As(err, &sfe):
		r.setReadyConditionFalse(ctx, m, "SourceFetchError", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "SourceFetchError", "Reconcile", err.Error())
		return ctrl.Result{}, err

	default:
		r.setReadyConditionFalse(ctx, m, "ReconcileError", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", err.Error())
		return ctrl.Result{}, err
	}
}

// setReadyConditionFalse updates the Ready condition to False via status patch.
func (r *ModuleReconciler) setReadyConditionFalse(ctx context.Context, m *modulev1alpha1.Module, reason, message string) {
	logger := log.FromContext(ctx)

	patch := client.MergeFrom(m.DeepCopy())
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               mod.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: m.Generation,
	})
	m.Status.ObservedGeneration = m.Generation

	if err := r.Status().Patch(ctx, m, patch); err != nil {
		logger.Error(err, "Failed to patch Ready=False status condition", "reason", reason)
	}
}

// updateStatus calculates and patches the Module status based on the reconcile
// outcome and upgrade decision. It directly uses the reconcile result instead
// of observing external resources.
func (r *ModuleReconciler) updateStatus(
	ctx context.Context,
	m *modulev1alpha1.Module,
	mc *modulev1alpha1.ModuleClass,
	decision mod.UpgradeDecision,
	result *reconcileResult,
) error {
	newStatus := m.Status.DeepCopy()
	newStatus.ObservedGeneration = m.Generation
	newStatus.AvailableClassGeneration = mc.Generation

	targetNS := mod.TargetNamespace(m, mc)
	newStatus.Namespace = targetNS

	switch decision {
	case mod.UpgradeInitialInstall, mod.UpgradeApproved:
		newStatus.AppliedClassGeneration = mc.Generation
	case mod.UpgradeNotNeeded:
		// keep existing AppliedClassGeneration
	case mod.UpgradePending:
		// don't update AppliedClassGeneration
	}

	if result != nil {
		switch {
		case result.Helm != nil:
			newStatus.HelmRelease = &modulev1alpha1.HelmReleaseStatus{
				ChartVersion:   result.Helm.ChartVersion,
				Revision:       result.Helm.Revision,
				Status:         result.Helm.Status,
				ValuesChecksum: result.Helm.ValuesChecksum,
			}
			newStatus.Kustomization = nil
			newStatus.Inventory = nil

			meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
				Type:               mod.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "HelmReleaseReady",
				Message:            fmt.Sprintf("Helm release %s (chart %s, rev %d)", result.Helm.Status, result.Helm.ChartVersion, result.Helm.Revision),
				ObservedGeneration: m.Generation,
			})

		case result.Kustomize != nil:
			newStatus.Kustomization = &modulev1alpha1.KustomizationStatus{
				LastAppliedRevision:   result.Kustomize.LastAppliedRevision,
				LastAttemptedRevision: result.Kustomize.LastAppliedRevision,
			}
			newStatus.HelmRelease = nil
			newStatus.Inventory = result.Kustomize.Inventory

			meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
				Type:               mod.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "KustomizationReady",
				Message:            fmt.Sprintf("Applied revision %s", result.Kustomize.LastAppliedRevision),
				ObservedGeneration: m.Generation,
			})
		}
	}

	if decision == mod.UpgradePending {
		meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
			Type:               mod.ConditionTypeUpgradeAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "UpgradePending",
			Message:            fmt.Sprintf("Class generation %d available (applied: %d)", mc.Generation, newStatus.AppliedClassGeneration),
			ObservedGeneration: m.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&newStatus.Conditions, mod.ConditionTypeUpgradeAvailable)
	}

	slices.SortFunc(newStatus.Conditions, func(a, b metav1.Condition) int {
		return cmp.Compare(a.Type, b.Type)
	})

	if !equality.Semantic.DeepEqual(m.Status, *newStatus) {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status = *newStatus
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Module status updated")
		r.Recorder.Eventf(m, nil, corev1.EventTypeNormal, "Reconciled", "Reconcile",
			"Module resources reconciled for class %s", m.Spec.ModuleClassName)
	}

	return nil
}

// requeueInterval returns the RequeueAfter based on the class's interval.
func (r *ModuleReconciler) requeueInterval(mc *modulev1alpha1.ModuleClass) ctrl.Result {
	switch {
	case mc.Spec.HelmChart != nil:
		return ctrl.Result{RequeueAfter: mc.Spec.HelmChart.Interval.Duration}
	case mc.Spec.Kustomization != nil:
		return ctrl.Result{RequeueAfter: mc.Spec.Kustomization.Interval.Duration}
	default:
		return ctrl.Result{}
	}
}

// SetupWithManager registers the controller with the Manager and defines watches.
func (r *ModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&modulev1alpha1.Module{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&modulev1alpha1.ModuleClass{},
			handler.EnqueueRequestsFromMapFunc(r.mapModuleClassToModules),
		).
		Named("module").
		Complete(r)
}

// mapModuleClassToModules enqueues all Modules that reference the changed ModuleClass.
func (r *ModuleReconciler) mapModuleClassToModules(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("class-watch")
	className := obj.GetName()

	var modules modulev1alpha1.ModuleList
	if err := r.List(ctx, &modules); err != nil {
		logger.Error(err, "Failed to list Modules for ModuleClass change re-enqueue")
		return nil
	}

	var requests []reconcile.Request
	for _, m := range modules.Items {
		if m.Spec.ModuleClassName == className {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: m.Name},
			})
		}
	}

	if len(requests) > 0 {
		logger.Info("ModuleClass changed, re-enqueuing referencing Modules",
			"class", className, "count", len(requests))
	}
	return requests
}
