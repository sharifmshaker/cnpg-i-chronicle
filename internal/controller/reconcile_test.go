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
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

func storeRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "config-store"}}
}

func readStore(t *testing.T, kube client.Client) *chroniclev1.ConfigStore {
	t.Helper()
	var got chroniclev1.ConfigStore
	if err := kube.Get(context.Background(), storeRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// checkFails is an empty store whose reachability probe reports err.
func checkFails(err error) *storetest.Backend {
	backend := storetest.NewBackend()
	backend.CheckErr = err
	return backend
}

func condition(conditions []metav1.Condition, kind string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == kind {
			return &conditions[i]
		}
	}
	return nil
}

// --- ConfigStore ------------------------------------------------------

func TestConfigStoreReconcilePublishesResolvedConfiguration(t *testing.T) {
	reconciler, kube := newReconciler(t, emptyStore())
	reconciler.Resolver = storetest.Provider{Store: storetest.NewBackend()}

	result, err := reconciler.Reconcile(context.Background(), storeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != resyncInterval {
		t.Errorf("RequeueAfter = %v, want the resync interval %v", result.RequeueAfter, resyncInterval)
	}

	got := readStore(t, kube)
	if got.Status.Resolved == nil {
		t.Fatal("status.resolved was not published")
	}
	if got.Status.Resolved.DestinationPath != "s3://backups/" {
		t.Errorf("destinationPath = %q", got.Status.Resolved.DestinationPath)
	}
	if got.Status.Resolved.Source != "Inline" {
		t.Errorf("source = %q, want Inline", got.Status.Resolved.Source)
	}

	ready := condition(got.Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want True", ready)
	}
}

// A store that cannot be resolved must say why, and must not keep a stale
// resolved block around implying it still works.
func TestConfigStoreReconcileReportsResolutionFailure(t *testing.T) {
	reconciler, kube := newReconciler(t, emptyStore())
	reconciler.Resolver = storetest.Provider{ResolveErr: errors.New("no such ObjectStore")}

	if _, err := reconciler.Reconcile(context.Background(), storeRequest()); err != nil {
		t.Fatalf("a resolution failure should be reported in status, not returned: %v", err)
	}

	got := readStore(t, kube)
	if got.Status.Resolved != nil {
		t.Error("status.resolved should be cleared when resolution fails")
	}
	ready := condition(got.Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %+v, want False", ready)
	}
	if !strings.Contains(ready.Message, "no such ObjectStore") {
		t.Errorf("message does not carry the cause: %q", ready.Message)
	}
}

// The CRD-absent case gets its own reason so an operator can tell "install the
// barman plugin" apart from "your config is wrong".
func TestConfigStoreReconcileDistinguishesMissingCRD(t *testing.T) {
	reconciler, kube := newReconciler(t, emptyStore())
	reconciler.Resolver = storetest.Provider{
		ResolveErr: &store.ObjectStoreCRDAbsentError{Err: errors.New("no matches for kind")},
	}

	if _, err := reconciler.Reconcile(context.Background(), storeRequest()); err != nil {
		t.Fatal(err)
	}
	ready := condition(readStore(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Reason != chroniclev1.ReasonObjectStoreCRDAbsent {
		t.Fatalf("reason = %v, want %s", ready, chroniclev1.ReasonObjectStoreCRDAbsent)
	}
}

// Ready has to mean "capture will work", not "the spec parses". Each case
// below used to report Ready and first surface as PhaseFailurePlugin on the
// first Cluster that tried to capture.
func TestConfigStoreReadyReflectsCredentialsAndReachability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   storetest.Provider
		wantStatus metav1.ConditionStatus
		wantReason string
		wantText   string
	}{
		{
			name:       "credentials Secret cannot be read",
			provider:   storetest.Provider{BackendErr: errors.New(`secret "creds" not found`)},
			wantStatus: metav1.ConditionFalse,
			wantReason: chroniclev1.ReasonConfigurationInvalid,
			wantText:   `secret "creds" not found`,
		},
		{
			name:       "object store rejects the probe",
			provider:   storetest.Provider{Store: checkFails(errors.New("cannot list s3://backups/: AccessDenied: Access Denied"))},
			wantStatus: metav1.ConditionFalse,
			wantReason: chroniclev1.ReasonObjectStoreUnreachable,
			wantText:   "AccessDenied",
		},
		{
			name:       "bucket does not exist yet",
			provider:   storetest.Provider{Store: checkFails(fmt.Errorf("%w: backups", store.ErrBucketNotFound))},
			wantStatus: metav1.ConditionTrue,
			wantReason: chroniclev1.ReasonResolved,
			wantText:   "created on first write",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, kube := newReconciler(t, emptyStore())
			reconciler.Resolver = tc.provider

			result, err := reconciler.Reconcile(context.Background(), storeRequest())
			if err != nil {
				t.Fatalf("this belongs in status, not an error: %v", err)
			}

			ready := condition(readStore(t, kube).Status.Conditions, chroniclev1.ConditionReady)
			if ready == nil || ready.Status != tc.wantStatus || ready.Reason != tc.wantReason {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tc.wantStatus, tc.wantReason)
			}
			if !strings.Contains(ready.Message, tc.wantText) {
				t.Errorf("message should mention %q; got: %s", tc.wantText, ready.Message)
			}

			// A store that is not usable is retried sooner than the resync, so
			// fixing the cause shows up promptly.
			wantRequeue := resyncInterval
			if tc.wantStatus == metav1.ConditionFalse {
				wantRequeue = notReadyRetryInterval
			}
			if result.RequeueAfter != wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, wantRequeue)
			}
		})
	}
}

func TestConfigStoreReconcileIgnoresMissingStore(t *testing.T) {
	reconciler, _ := newReconciler(t)
	reconciler.Resolver = storetest.Provider{Store: storetest.NewBackend()}

	if _, err := reconciler.Reconcile(context.Background(), storeRequest()); err != nil {
		t.Fatalf("a deleted store should not error: %v", err)
	}
}

// --- RestorePolicy ----------------------------------------------------

func restorePolicy(mode chroniclev1.SelectMode, transforms ...chroniclev1.TransformRule) *chroniclev1.RestorePolicy {
	return &chroniclev1.RestorePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "from-prod", Namespace: "default"},
		Spec: chroniclev1.RestorePolicySpec{
			Select:    chroniclev1.RestoreSelect{Mode: mode},
			Transform: transforms,
		},
	}
}

func restoreRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "from-prod"}}
}

func readPolicy(t *testing.T, kube client.Client) *chroniclev1.RestorePolicy {
	t.Helper()
	var got chroniclev1.RestorePolicy
	if err := kube.Get(context.Background(), restoreRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func newRestoreReconciler(
	t *testing.T, objects ...client.Object,
) (*RestorePolicyReconciler, client.Client) {
	t.Helper()
	base, kube := newReconciler(t, objects...)
	return &RestorePolicyReconciler{Client: base.Client}, kube
}

func TestRestorePolicyReconcileIgnoresMissingPolicy(t *testing.T) {
	reconciler, _ := newRestoreReconciler(t)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatalf("a deleted policy should not error: %v", err)
	}
}

// The ordinary case: rules that are internally consistent and name paths that
// could be captured. There is no store to consult, so Ready=True is a statement
// about the rules alone.
func TestRestorePolicyReconcileAcceptsValidRules(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
		Path: "spec.storage.size", Expression: "old.divide(2)",
	})
	policy.Spec.Skip = []chroniclev1.SkipRule{
		{Group: "Monitoring"},
		{Path: "spec.postgresql.parameters.work_mem"},
	}

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", ready)
	}
}

// A path that is both skipped and transformed is individually valid on each
// side — the skip parses, the expression compiles — so nothing catches it until
// a plan is built. The pre-flight check exists precisely to catch that class of
// mistake before a cluster is ever created from the policy.
func TestRestorePolicyReconcileRejectsSkippedAndTransformedPath(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
		Path: "spec.storage.size", Expression: "old.divide(2)",
	})
	policy.Spec.Skip = []chroniclev1.SkipRule{{Path: "spec.storage.size"}}

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	for _, want := range []string{"both transformed and skipped", "spec.storage.size"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("message should mention %q; got: %s", want, ready.Message)
		}
	}
}

// A transform naming a path the snapshot never captured is almost always a
// typo, and silently doing nothing is the worst possible response to one.
//
// The path is misspelled against CloudNativePG's own types, so this is caught
// statically and the message names the field that does not exist.
func TestRestorePolicyReconcileRejectsTransformOnAbsentPath(t *testing.T) {
	reconciler, kube := newRestoreReconciler(t,
		restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
			Path: "spec.storage.sizze", Expression: "old.divide(2)", // typo
		}))
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	for _, want := range []string{"spec.storage.sizze", "no such field"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("message should mention %q; got: %s", want, ready.Message)
		}
	}
}

// onMissing: Ignore says a path may be absent from some histories. It does not
// say the path may be nonsense — the spelling check still applies, or a typo
// would buy silence instead of an error.
func TestRestorePolicyReconcileStillRejectsBadPathsWithOnMissingIgnore(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
		Path:       "spec.storage.sizze",
		Expression: "old.divide(2)",
		OnMissing:  chroniclev1.MissingIgnore,
	})

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False: Ignore excuses absence, not a misspelling", ready)
	}
}

// A path that resolves against ClusterSpec but that the allowlist never carries
// can appear in no snapshot either, and the reason a person needs to hear is a
// different one: the field is real, it is simply never captured.
func TestRestorePolicyReconcileRejectsUncapturedPath(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
		Path: "spec.bootstrap.initdb.database", Expression: "old",
	})

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	if !strings.Contains(ready.Message, "denylist") {
		t.Errorf("message should explain why the path is unusable; got: %s", ready.Message)
	}
}

// A keyed rule on something that is not a map can never do what it says, so it
// is reported on the policy rather than discovered on a restore.
func TestRestorePolicyReconcileRejectsKeyedRuleOnANonMap(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest)
	policy.Spec.Skip = []chroniclev1.SkipRule{{Path: "spec.storage.size", KeyPrefix: "x"}}

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}
	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, "only to a path naming a map") {
		t.Fatalf("Ready = %+v, want False explaining keyPrefix needs a map", ready)
	}
}

// A capture group naming nothing is a typo, and a typo silently narrows what
// every cluster writing to this store captures. It has to be visible on the
// store, with the valid names, rather than surfacing later as a capture error.
func TestConfigStoreReconcileRejectsUnknownCaptureGroups(t *testing.T) {
	configStore := emptyStore()
	configStore.Spec.CaptureGroups = []string{"Gucs", "Guccis"}

	reconciler, kube := newReconciler(t, configStore)
	reconciler.Resolver = storetest.Provider{Store: storetest.NewBackend()}

	if _, err := reconciler.Reconcile(context.Background(), storeRequest()); err != nil {
		t.Fatalf("a bad group should be reported in status, not returned: %v", err)
	}

	var got chroniclev1.ConfigStore
	if err := kube.Get(context.Background(), storeRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	ready := condition(got.Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	for _, want := range []string{"Guccis", "Gucs"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("message should name %q (the bad group, and the valid list); got: %s",
				want, ready.Message)
		}
	}
}

// The effective set is reported so that "empty means default" is visible.
func TestConfigStoreReconcileReportsTheEffectiveCaptureGroups(t *testing.T) {
	reconciler, kube := newReconciler(t, emptyStore())
	reconciler.Resolver = storetest.Provider{Store: storetest.NewBackend()}

	if _, err := reconciler.Reconcile(context.Background(), storeRequest()); err != nil {
		t.Fatal(err)
	}

	var got chroniclev1.ConfigStore
	if err := kube.Get(context.Background(), storeRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Resolved == nil {
		t.Fatal("status.resolved is nil")
	}
	if len(got.Status.Resolved.CaptureGroups) != len(snapshot.DefaultGroupNames()) {
		t.Errorf("an unset spec.captureGroups should report the default set, got %v",
			got.Status.Resolved.CaptureGroups)
	}
}

// The same holds when the skip names a group rather than the exact path: skip
// rules resolve group names to paths without needing any history.
func TestRestorePolicyRejectsTransformOfAGroupSkippedWholesale(t *testing.T) {
	policy := restorePolicy(chroniclev1.SelectLatest, chroniclev1.TransformRule{
		Path: "spec.storage.size", Expression: "old.divide(2)",
	})
	policy.Spec.Skip = []chroniclev1.SkipRule{{Group: "Storage"}}

	reconciler, kube := newRestoreReconciler(t, policy)
	if _, err := reconciler.Reconcile(context.Background(), restoreRequest()); err != nil {
		t.Fatal(err)
	}

	ready := condition(readPolicy(t, kube).Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False: the Storage group covers spec.storage.size", ready)
	}
}
