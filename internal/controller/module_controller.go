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

	mod "github.com/otterscale/addons-operator/internal/module"
	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
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
// +kubebuilder:rbac:groups=addons.otterscale.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=addons.otterscale.io,resources=modules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=addons.otterscale.io,resources=modules/finalizers,verbs=update
// +kubebuilder:rbac:groups=addons.otterscale.io,resources=moduletemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main loop for the Module controller.
// It implements the level-triggered reconciliation logic:
// Fetch -> Finalizer -> Fetch Template -> Check Upgrade -> Reconcile Resources -> Status Update.
func (r *ModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.Name)
	ctx = log.IntoContext(ctx, logger)

	var m addonsv1alpha1.Module
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

	mt, err := r.fetchModuleTemplate(ctx, m.Spec.TemplateRef)
	if err != nil {
		return r.handleReconcileError(ctx, &m, err)
	}

	decision := mod.CheckUpgrade(&m, mt)

	var requeueAfter ctrl.Result

	if decision.ShouldApply() {
		result, reconcileErr := r.reconcileResources(ctx, &m, mt)
		if reconcileErr != nil {
			return r.handleReconcileError(ctx, &m, reconcileErr)
		}
		if err := r.updateStatus(ctx, &m, mt, decision, result); err != nil {
			return ctrl.Result{}, err
		}
		requeueAfter = r.requeueInterval(mt)
	} else {
		logger.Info("Upgrade pending approval",
			"available", mt.Generation,
			"applied", m.Status.AppliedTemplateGeneration)
		if err := r.updateStatus(ctx, &m, mt, decision, nil); err != nil {
			return ctrl.Result{}, err
		}
		requeueAfter = r.requeueInterval(mt)
	}

	return requeueAfter, nil
}

// fetchModuleTemplate retrieves the ModuleTemplate referenced by the Module.
func (r *ModuleReconciler) fetchModuleTemplate(ctx context.Context, name string) (*addonsv1alpha1.ModuleTemplate, error) {
	var mt addonsv1alpha1.ModuleTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &mt); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &mod.TemplateNotFoundError{Name: name}
		}
		return nil, err
	}
	return &mt, nil
}

// reconcileResult is a union type carrying the outcome of either a Helm
// or Kustomize reconciliation.
type reconcileResult struct {
	Helm      *mod.HelmReconcileResult
	Kustomize *mod.KustomizeReconcileResult
}

// reconcileResources dispatches to the appropriate domain function
// based on the template type (HelmChart or Kustomization).
func (r *ModuleReconciler) reconcileResources(ctx context.Context, m *addonsv1alpha1.Module, mt *addonsv1alpha1.ModuleTemplate) (*reconcileResult, error) {
	switch {
	case mt.Spec.HelmChart != nil:
		res, err := mod.ReconcileHelmChart(ctx, r.Client, r.RestConfig, m, mt, r.Version)
		if err != nil {
			return nil, err
		}
		return &reconcileResult{Helm: res}, nil
	case mt.Spec.Kustomization != nil:
		res, err := mod.ReconcileKustomization(ctx, r.Client, r.RestConfig, m, mt, r.Version)
		if err != nil {
			return nil, err
		}
		return &reconcileResult{Kustomize: res}, nil
	default:
		return nil, &mod.TemplateInvalidError{
			Name:    mt.Name,
			Message: "neither helmChart nor kustomization is defined",
		}
	}
}

// reconcileDelete handles the deletion flow:
// 1. Clean up managed resources (Helm uninstall or Kustomize prune)
// 2. Remove the Finalizer
func (r *ModuleReconciler) reconcileDelete(ctx context.Context, m *addonsv1alpha1.Module) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if ctrlutil.ContainsFinalizer(m, mod.ModuleFinalizer) {
		logger.Info("Cleaning up managed resources before Module deletion")

		if m.Status.HelmRelease != nil {
			namespace := r.resolveCleanupNamespace(ctx, m)
			releaseName := m.Name
			mt, err := r.fetchModuleTemplate(ctx, m.Spec.TemplateRef)
			if err == nil && mt.Spec.HelmChart != nil && mt.Spec.HelmChart.ReleaseName != "" {
				releaseName = mt.Spec.HelmChart.ReleaseName
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
func (r *ModuleReconciler) resolveCleanupNamespace(ctx context.Context, m *addonsv1alpha1.Module) string {
	if m.Status.Namespace != "" {
		return m.Status.Namespace
	}
	if m.Spec.Namespace != nil {
		return *m.Spec.Namespace
	}
	mt, err := r.fetchModuleTemplate(ctx, m.Spec.TemplateRef)
	if err != nil {
		return ""
	}
	return mt.Spec.Namespace
}

// handleReconcileError categorizes errors and updates status accordingly.
// Permanent errors (TemplateNotFound, TemplateInvalid) do NOT requeue.
// Transient errors are returned to controller-runtime for exponential backoff retry.
func (r *ModuleReconciler) handleReconcileError(ctx context.Context, m *addonsv1alpha1.Module, err error) (ctrl.Result, error) {
	var tnf *mod.TemplateNotFoundError
	var tie *mod.TemplateInvalidError
	var cfe *mod.ChartFetchError
	var sfe *mod.SourceFetchError

	switch {
	case errors.As(err, &tnf):
		r.setReadyConditionFalse(ctx, m, "TemplateNotFound", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "TemplateNotFound", "Reconcile", err.Error())
		return ctrl.Result{}, nil

	case errors.As(err, &tie):
		r.setReadyConditionFalse(ctx, m, "TemplateInvalid", err.Error())
		r.Recorder.Eventf(m, nil, corev1.EventTypeWarning, "TemplateInvalid", "Reconcile", err.Error())
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
func (r *ModuleReconciler) setReadyConditionFalse(ctx context.Context, m *addonsv1alpha1.Module, reason, message string) {
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
	m *addonsv1alpha1.Module,
	mt *addonsv1alpha1.ModuleTemplate,
	decision mod.UpgradeDecision,
	result *reconcileResult,
) error {
	newStatus := m.Status.DeepCopy()
	newStatus.ObservedGeneration = m.Generation
	newStatus.AvailableTemplateGeneration = mt.Generation

	targetNS := mod.TargetNamespace(m, mt)
	newStatus.Namespace = targetNS

	switch decision {
	case mod.UpgradeInitialInstall, mod.UpgradeApproved:
		newStatus.AppliedTemplateGeneration = mt.Generation
	case mod.UpgradeNotNeeded:
		// keep existing AppliedTemplateGeneration
	case mod.UpgradePending:
		// don't update AppliedTemplateGeneration
	}

	if result != nil {
		switch {
		case result.Helm != nil:
			newStatus.HelmRelease = &addonsv1alpha1.HelmReleaseStatus{
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
			newStatus.Kustomization = &addonsv1alpha1.KustomizationStatus{
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
			Message:            fmt.Sprintf("Template generation %d available (applied: %d)", mt.Generation, newStatus.AppliedTemplateGeneration),
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
			"Module resources reconciled for template %s", m.Spec.TemplateRef)
	}

	return nil
}

// requeueInterval returns the RequeueAfter based on the template's interval.
func (r *ModuleReconciler) requeueInterval(mt *addonsv1alpha1.ModuleTemplate) ctrl.Result {
	switch {
	case mt.Spec.HelmChart != nil:
		return ctrl.Result{RequeueAfter: mt.Spec.HelmChart.Interval.Duration}
	case mt.Spec.Kustomization != nil:
		return ctrl.Result{RequeueAfter: mt.Spec.Kustomization.Interval.Duration}
	default:
		return ctrl.Result{}
	}
}

// SetupWithManager registers the controller with the Manager and defines watches.
func (r *ModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.Module{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&addonsv1alpha1.ModuleTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.mapModuleTemplateToModules),
		).
		Named("module").
		Complete(r)
}

// mapModuleTemplateToModules enqueues all Modules that reference the changed ModuleTemplate.
func (r *ModuleReconciler) mapModuleTemplateToModules(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("template-watch")
	templateName := obj.GetName()

	var modules addonsv1alpha1.ModuleList
	if err := r.List(ctx, &modules); err != nil {
		logger.Error(err, "Failed to list Modules for ModuleTemplate change re-enqueue")
		return nil
	}

	var requests []reconcile.Request
	for _, m := range modules.Items {
		if m.Spec.TemplateRef == templateName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: m.Name},
			})
		}
	}

	if len(requests) > 0 {
		logger.Info("ModuleTemplate changed, re-enqueuing referencing Modules",
			"template", templateName, "count", len(requests))
	}
	return requests
}
