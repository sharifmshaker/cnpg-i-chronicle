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

package operator

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/cnpg-i/pkg/operator"
	"github.com/cloudnative-pg/cnpg-i/pkg/reconciler"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

func statusRequest(t *testing.T, cluster any) *operator.SetStatusInClusterRequest {
	t.Helper()
	encoded, err := json.Marshal(cluster)
	if err != nil {
		t.Fatal(err)
	}
	return &operator.SetStatusInClusterRequest{Cluster: encoded}
}

// The status is what makes "is my configuration being captured?" answerable from
// the Cluster, which is the object an operator is already looking at.
func TestSetStatusInClusterReportsTheWatermark(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())

	// The watermark comes from the store, so put something there to report.
	if _, err := impl.Post(ctx, request(t, testCluster(7))); err != nil {
		t.Fatal(err)
	}

	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}

	response, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(7)))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.JsonStatus) == 0 {
		t.Fatal("no status was returned")
	}

	var status ClusterStatus
	if err := json.Unmarshal(response.JsonStatus, &status); err != nil {
		t.Fatal(err)
	}
	if status.LastSavedGeneration != 7 {
		t.Errorf("lastSavedGeneration = %d, want 7", status.LastSavedGeneration)
	}
	if status.Store != "config-store" || status.ServerName != "pg-source" {
		t.Errorf("status = %+v", status)
	}
	// The two checksums are what the capture hook compares against, so a status
	// missing them would send every reconcile down the slow path.
	if status.Checksum == "" || status.MetadataChecksum == "" {
		t.Errorf("status carries no checksums, so it cannot serve as a watermark: %+v", status)
	}
}

// The response has to be a pure function of observed state: CloudNativePG
// requeues the cluster five seconds after any plugin status change, so anything
// varying between calls becomes a permanent reconcile loop.
func TestSetStatusInClusterIsStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())
	if _, err := impl.Post(ctx, request(t, testCluster(3))); err != nil {
		t.Fatal(err)
	}
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}

	first, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(3)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(3)))
		if err != nil {
			t.Fatal(err)
		}
		if string(next.JsonStatus) != string(first.JsonStatus) {
			t.Fatalf("status changed between identical calls:\n  %s\n  %s",
				first.JsonStatus, next.JsonStatus)
		}
	}
}

// withPublishedStatus returns the cluster as CloudNativePG hands it back once
// it has written the plugin's status: the watermark from the previous call is
// already in .status.pluginStatus.
func withPublishedStatus(cluster *apiv1.Cluster, status []byte) *apiv1.Cluster {
	cluster.Status.PluginStatus = []apiv1.PluginStatus{{
		Name:   metadata.PluginName,
		Status: string(status),
	}}
	return cluster
}

// CloudNativePG polls this every five seconds for as long as a plugin reports a
// status. Re-deriving the watermark each time would be two object GETs per
// cluster per poll, for an answer the Cluster is already carrying.
func TestSetStatusInClusterReadsNothingWhenTheWatermarkHolds(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())
	if _, err := impl.Post(ctx, request(t, testCluster(7))); err != nil {
		t.Fatal(err)
	}
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}

	first, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(7)))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.JsonStatus) == 0 {
		t.Fatal("no status was returned")
	}

	settled := withPublishedStatus(testCluster(7), first.JsonStatus)
	backend.Gets = 0
	response, err := implementation.SetStatusInCluster(ctx, statusRequest(t, settled))
	if err != nil {
		t.Fatal(err)
	}
	if backend.Gets != 0 {
		t.Errorf("%d object reads for a cluster whose watermark still holds, want 0", backend.Gets)
	}
	// A no-op leaves the value already on the Cluster, which is this same
	// watermark: publishing it again would be a write for no change.
	if len(response.JsonStatus) != 0 {
		t.Errorf("republished an unchanged watermark: %s", response.JsonStatus)
	}
}

// The short-circuit above must not outlive the state it was computed from.
// Generation covers spec edits; the fingerprint covers label and annotation
// edits, which Kubernetes does not bump the generation for.
func TestSetStatusInClusterRefreshesWhenTheClusterMoves(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())
	if _, err := impl.Post(ctx, request(t, testCluster(7))); err != nil {
		t.Fatal(err)
	}
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}

	published, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(7)))
	if err != nil {
		t.Fatal(err)
	}

	moved := map[string]*apiv1.Cluster{
		"spec edit": testCluster(8),
		"label edit": func() *apiv1.Cluster {
			cluster := testCluster(7)
			cluster.Labels = map[string]string{"team": "platform"}
			return cluster
		}(),
	}
	for name, cluster := range moved {
		t.Run(name, func(t *testing.T) {
			backend.Gets = 0
			response, err := implementation.SetStatusInCluster(
				ctx, statusRequest(t, withPublishedStatus(cluster, published.JsonStatus)))
			if err != nil {
				t.Fatal(err)
			}
			if backend.Gets == 0 {
				t.Error("the store was never consulted, so the watermark was trusted past the state it describes")
			}
			if len(response.JsonStatus) == 0 {
				t.Error("no status was returned for a cluster that moved")
			}
		})
	}
}

// A generation bump that changes nothing captured leaves the watermark on the
// older snapshot's generation for good. Once the capture hook has confirmed
// that state, the status hook must stop going back to the store for it:
// CloudNativePG polls every five seconds, indefinitely.
func TestSetStatusInClusterSettlesAfterAGenerationBumpThatCapturedNothing(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())
	if _, err := impl.Post(ctx, request(t, testCluster(1))); err != nil {
		t.Fatal(err)
	}
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}
	published, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(1)))
	if err != nil {
		t.Fatal(err)
	}

	bumped := func() *apiv1.Cluster {
		cluster := testCluster(2)
		cluster.Spec.Backup = &apiv1.BackupConfiguration{RetentionPolicy: "7d"}
		return withPublishedStatus(cluster, published.JsonStatus)
	}
	if _, err := impl.Post(ctx, request(t, bumped())); err != nil {
		t.Fatal(err)
	}

	backend.Gets = 0
	for i := 0; i < 5; i++ {
		response, err := implementation.SetStatusInCluster(ctx, statusRequest(t, bumped()))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.JsonStatus) != 0 {
			t.Fatalf("republished an unchanged watermark: %s", response.JsonStatus)
		}
	}
	if backend.Gets != 0 {
		t.Errorf("%d object reads across 5 polls of a settled cluster, want 0", backend.Gets)
	}
}

// The settled cache must not hide a capture from the status hook. After a new
// snapshot is written the cache holds that snapshot's key, the watermark on the
// Cluster still names the old one, and the new one has to be published.
func TestSetStatusInClusterPublishesACaptureTheCacheKnowsAbout(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())
	if _, err := impl.Post(ctx, request(t, testCluster(1))); err != nil {
		t.Fatal(err)
	}
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}
	published, err := implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(1)))
	if err != nil {
		t.Fatal(err)
	}

	changed := testCluster(2)
	changed.Spec.PostgresConfiguration.Parameters["shared_buffers"] = "256MB"
	if _, err := impl.Post(ctx, request(t, withPublishedStatus(changed, published.JsonStatus))); err != nil {
		t.Fatal(err)
	}

	response, err := implementation.SetStatusInCluster(ctx, statusRequest(t, withPublishedStatus(changed, published.JsonStatus)))
	if err != nil {
		t.Fatal(err)
	}
	var status ClusterStatus
	if err := json.Unmarshal(response.JsonStatus, &status); err != nil {
		t.Fatalf("no new watermark was published: %v", err)
	}
	if status.LastSavedGeneration != 2 {
		t.Errorf("lastSavedGeneration = %d, want the new capture's 2", status.LastSavedGeneration)
	}
}

func TestSetStatusInClusterNoOpsWhenNothingToReport(t *testing.T) {
	ctx := context.Background()

	// Save disabled.
	impl, backend, kube := newHarness(t, testConfigStore())
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}
	cluster := testCluster(1)
	cluster.Spec.Plugins = nil
	response, err := implementation.SetStatusInCluster(ctx, statusRequest(t, cluster))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.JsonStatus) != 0 {
		t.Errorf("a cluster not using the plugin got a status: %s", response.JsonStatus)
	}

	// The store exists but holds nothing for this cluster yet.
	response, err = implementation.SetStatusInCluster(ctx, statusRequest(t, testCluster(1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.JsonStatus) != 0 {
		t.Errorf("an uncaptured cluster got a status: %s", response.JsonStatus)
	}
}

func TestSetStatusInClusterToleratesMissingStore(t *testing.T) {
	impl, backend, kube := newHarness(t) // no ConfigStore
	implementation := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}

	response, err := implementation.SetStatusInCluster(
		context.Background(), statusRequest(t, testCluster(1)))
	if err != nil {
		t.Fatalf("a missing store should be a no-op, not an error: %v", err)
	}
	if len(response.JsonStatus) != 0 {
		t.Errorf("got a status for a missing store: %s", response.JsonStatus)
	}
}

// --- capabilities -------------------------------------------------------

// Publishing the watermark is what makes the capture state visible to
// `kubectl get cluster`, and it is also the capture hook's cheapest fast path:
// without this capability CloudNativePG never calls SetStatusInCluster, the
// Cluster carries no watermark, and every reconcile falls through to the object
// store instead.
func TestAdvertisesSetStatusInCluster(t *testing.T) {
	result, err := OperatorImplementation{}.GetCapabilities(
		context.Background(), &operator.OperatorCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var rpcs []operator.OperatorCapability_RPC_Type
	for _, capability := range result.Capabilities {
		rpcs = append(rpcs, capability.GetRpc().GetType())
	}
	if !slices.Contains(rpcs, operator.OperatorCapability_RPC_TYPE_SET_STATUS_IN_CLUSTER) {
		t.Errorf("operator capabilities = %v, want SET_STATUS_IN_CLUSTER — without "+
			"it the watermark is never published and the fast path is dead code", rpcs)
	}
}

// The reconciler hooks are what actually drive capture, and they are separate
// from the operator capabilities above: capture must keep working whether or
// not the watermark is published.
func TestAdvertisesClusterReconcilerHooks(t *testing.T) {
	result, err := ReconcilerImplementation{}.GetCapabilities(
		context.Background(), &reconciler.ReconcilerHooksCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var kinds []reconciler.ReconcilerHooksCapability_Kind
	for _, capability := range result.ReconcilerCapabilities {
		kinds = append(kinds, capability.Kind)
	}
	if !slices.Contains(kinds, reconciler.ReconcilerHooksCapability_KIND_CLUSTER) {
		t.Errorf("reconciler capabilities = %v, want KIND_CLUSTER — without it "+
			"the plugin is never called and nothing is ever captured", kinds)
	}
}
