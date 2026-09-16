/*
Copyright 2026 The cnpg-i-chronicle Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"context"

	"github.com/cloudnative-pg/machinery/pkg/log"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/restore"
)

// RestorePolicyReconciler pre-flights a restore policy.
//
// The restore itself happens in admission, where a failure blocks a cluster
// creation someone is watching. Running the same checks here, in the
// background, means a misspelled path or a contradictory rule shows up on
// `kubectl get restorepolicy` beforehand rather than at the moment it hurts.
//
// It reads no object store. A policy names no source — which history it is
// applied to is decided by the Cluster being created — so everything checkable
// here is a property of the rules, the capture allowlist, and CloudNativePG's
// own types. restore.CompileRules is that check, and the webhook runs the same
// one.
type RestorePolicyReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=chronicle.sharifmshaker.github.io,resources=restorepolicies/status,verbs=get;update;patch

// Reconcile checks a policy's rules and records the verdict.
func (r *RestorePolicyReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx).WithName("restorepolicy")

	var policy chroniclev1.RestorePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	original := policy.DeepCopy()
	condition := metav1.Condition{
		Type:    chroniclev1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  chroniclev1.ReasonResolved,
		Message: "Rules are valid",
	}
	if _, _, err := restore.CompileRules(policy.Spec.Skip, policy.Spec.Transform); err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = chroniclev1.ReasonConfigurationInvalid
		condition.Message = err.Error()
		contextLogger.Info("restore policy is not usable",
			"policy", req.NamespacedName, "reason", err.Error())
	}
	meta.SetStatusCondition(&policy.Status.Conditions, condition)

	if equalConditions(original.Status.Conditions, policy.Status.Conditions) {
		return ctrl.Result{}, nil
	}
	// A merge patch with no optimistic lock: this controller is the only writer
	// of RestorePolicy status, and the verdict is a pure function of the spec,
	// so two replicas racing here write the same bytes.
	if err := r.Status().Patch(ctx, &policy, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}

	// No resync. Nothing can change the verdict that this controller is not
	// already watching.
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller.
//
// Status writes are filtered out by the generation predicate: the verdict
// depends on the spec alone, so the update event this controller's own patch
// produces has nothing to tell it.
func (r *RestorePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&chroniclev1.RestorePolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("restorepolicy").
		Complete(r)
}
