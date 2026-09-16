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
	"strings"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/cnpg-i/pkg/reconciler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

// snapshotKeys returns only the versioned snapshot objects, excluding the
// latest pointer, so tests can count real captures.
func snapshotKeys(backend *storetest.Backend) []string {
	var keys []string
	for key := range backend.Objects {
		if strings.Contains(key, "/snapshots/") {
			keys = append(keys, key)
		}
	}
	return keys
}

// --- fixtures ----------------------------------------------------------

func testCluster(generation int64) *apiv1.Cluster {
	return &apiv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-source", Namespace: "default", Generation: generation,
		},
		Spec: apiv1.ClusterSpec{
			Instances:            3,
			StorageConfiguration: apiv1.StorageConfiguration{Size: "10Gi"},
			PostgresConfiguration: apiv1.PostgresConfiguration{
				Parameters: map[string]string{"shared_buffers": "128MB"},
			},
			Plugins: []apiv1.PluginConfiguration{{
				Name:       metadata.PluginName,
				Parameters: map[string]string{metadata.ParameterSaveToStore: "config-store"},
			}},
		},
	}
}

func testConfigStore() *chroniclev1.ConfigStore {
	return &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "config-store", Namespace: "default"},
		Spec: chroniclev1.ConfigStoreSpec{
			Prefix:        "chronicle",
			Configuration: ptr.To(storeConfiguration()),
		},
	}
}

func newHarness(t *testing.T, objects ...client.Object) (ReconcilerImplementation, *storetest.Backend, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := chroniclev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&chroniclev1.ConfigStore{}).
		Build()

	backend := storetest.NewBackend()
	// Wired as start.go wires it, so tests exercise the real configuration
	// rather than a nil cache that silently never hits.
	return ReconcilerImplementation{
		Client:   kube,
		Resolver: storetest.Provider{Store: backend},
		Settled:  newSettledCache(),
	}, backend, kube
}

func request(t *testing.T, cluster *apiv1.Cluster) *reconciler.ReconcilerHooksRequest {
	t.Helper()
	encoded, err := json.Marshal(cluster)
	if err != nil {
		t.Fatal(err)
	}
	// For KIND_CLUSTER the operator sets both fields to the same object.
	return &reconciler.ReconcilerHooksRequest{
		ClusterDefinition:  encoded,
		ResourceDefinition: encoded,
	}
}

// watermark reads what the plugin would report, from where it now lives: the
// object store. The Cluster's plugin status is a mirror of this, published by
// CloudNativePG rather than written by us, so the bucket is what a test can
// assert against directly.
func watermark(t *testing.T, backend *storetest.Backend) snapshot.Snapshot {
	t.Helper()
	raw, err := backend.Get(context.Background(),
		"pg-source/chronicle/latest.json")
	if err != nil {
		t.Fatalf("no latest.json: %v", err)
	}
	var latest snapshot.Snapshot
	if err := json.Unmarshal(raw, &latest); err != nil {
		t.Fatal(err)
	}
	return latest
}

// --- tests -------------------------------------------------------------

func TestPostCapturesFirstGeneration(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	result, err := impl.Post(ctx, request(t, testCluster(1)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Behavior != reconciler.ReconcilerHooksResult_BEHAVIOR_CONTINUE {
		t.Errorf("behavior = %v, want CONTINUE", result.Behavior)
	}

	if got := len(snapshotKeys(backend)); got != 1 {
		t.Fatalf("wrote %d snapshots, want 1", got)
	}
	latest := watermark(t, backend)
	if latest.Generation != 1 {
		t.Errorf("latest.json records generation %d, want 1", latest.Generation)
	}
	if latest.Checksum == "" {
		t.Error("the snapshot carries no checksum")
	}
	if latest.Cluster.Name != "pg-source" {
		t.Errorf("latest.json names cluster %q", latest.Cluster.Name)
	}
}

// Reconciliation runs constantly. Re-seeing a generation already captured must
// not touch the object store at all.
func TestPostIsIdempotentForTheSameGeneration(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	for i := 0; i < 5; i++ {
		if _, err := impl.Post(ctx, request(t, testCluster(1))); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(snapshotKeys(backend)); got != 1 {
		t.Errorf("wrote %d snapshots across 5 reconciles, want 1", got)
	}
	// One Put for the snapshot and one for the latest pointer, then nothing.
	if backend.Puts != 2 {
		t.Errorf("issued %d writes, want 2 (snapshot + latest pointer)", backend.Puts)
	}
}

// A generation bump that changed nothing we capture should advance the
// watermark without writing a byte-identical object again.
func TestPostSkipsWriteWhenNothingCapturedChanged(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	if _, err := impl.Post(ctx, request(t, testCluster(1))); err != nil {
		t.Fatal(err)
	}
	writesAfterFirst := backend.Puts

	// Generation moves because of a field outside every capture group.
	bumped := testCluster(2)
	bumped.Spec.Backup = &apiv1.BackupConfiguration{RetentionPolicy: "7d"}

	if _, err := impl.Post(ctx, request(t, bumped)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != writesAfterFirst {
		t.Errorf("wrote to the store for a change we do not capture (%d -> %d writes)",
			writesAfterFirst, backend.Puts)
	}

	// The watermark cannot record generation 2, because no snapshot was written
	// at generation 2 — the newest one still describes generation 1, correctly.
	if got := watermark(t, backend).Generation; got != 1 {
		t.Errorf("latest.json = generation %d; it should still describe the captured state", got)
	}

	// Which is why this state has to settle some other way. Without it every
	// later reconcile would re-extract and re-read the store to reach the same
	// conclusion, for as long as the cluster sits at this generation.
	readsBefore := backend.Gets
	for i := 0; i < 5; i++ {
		if _, err := impl.Post(ctx, request(t, bumped)); err != nil {
			t.Fatal(err)
		}
	}
	if backend.Puts != writesAfterFirst {
		t.Errorf("a settled state issued writes: %d -> %d", writesAfterFirst, backend.Puts)
	}
	if backend.Gets != readsBefore {
		t.Errorf("a settled state read the object store %d times; it should cost nothing",
			backend.Gets-readsBefore)
	}
}

func TestPostCapturesRealChange(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	if _, err := impl.Post(ctx, request(t, testCluster(1))); err != nil {
		t.Fatal(err)
	}
	first := watermark(t, backend).Checksum

	changed := testCluster(2)
	changed.Spec.PostgresConfiguration.Parameters["shared_buffers"] = "256MB"
	if _, err := impl.Post(ctx, request(t, changed)); err != nil {
		t.Fatal(err)
	}

	if got := len(snapshotKeys(backend)); got != 2 {
		t.Errorf("stored %d snapshots, want 2", got)
	}
	state := watermark(t, backend)
	if state.Checksum == first {
		t.Error("checksum did not change for a real GUC change")
	}
	if state.Generation != 2 {
		t.Errorf("latest.json records generation %d, want 2", state.Generation)
	}
}

func TestPostRequeuesWhenStoreMissing(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t) // no ConfigStore

	result, err := impl.Post(ctx, request(t, testCluster(1)))
	if err != nil {
		t.Fatalf("a missing store should requeue, not error: %v", err)
	}
	if result.Behavior != reconciler.ReconcilerHooksResult_BEHAVIOR_REQUEUE {
		t.Errorf("behavior = %v, want REQUEUE", result.Behavior)
	}
	if backend.Puts != 0 {
		t.Error("wrote to the store despite it not existing")
	}
}

func TestPostIgnoresClustersWithoutTheSaveParameter(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Spec.Plugins = nil

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != 0 {
		t.Error("captured a cluster that does not use the plugin")
	}
}

func TestPostRespectsDisabledPlugin(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Spec.Plugins[0].Enabled = ptr.To(false)

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != 0 {
		t.Error("captured a cluster whose plugin entry is disabled")
	}
}

// The guard is the only thing standing between "the webhook did not run" and a
// database quietly serving on the wrong configuration.
func TestPreRefusesWhenRestoreNeverRan(t *testing.T) {
	ctx := context.Background()
	impl, _, _ := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Spec.Plugins[0].Parameters[metadata.ParameterRestoreFromStore] = "from-prod"

	_, err := impl.Pre(ctx, request(t, cluster))
	if err == nil {
		t.Fatal("Pre allowed a cluster whose restore never happened")
	}
	for _, want := range []string{"restore", "from-prod", metadata.AnnotationRestoredFrom} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message should mention %q; got: %v", want, err)
		}
	}
}

func TestPreAllowsRestoredCluster(t *testing.T) {
	ctx := context.Background()
	impl, _, _ := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Spec.Plugins[0].Parameters[metadata.ParameterRestoreFromStore] = "from-prod"
	cluster.Annotations = map[string]string{
		metadata.AnnotationRestoredFrom: `{"key":"pg-source/chronicle/snapshots/x.json"}`,
	}

	result, err := impl.Pre(ctx, request(t, cluster))
	if err != nil {
		t.Fatal(err)
	}
	if result.Behavior != reconciler.ReconcilerHooksResult_BEHAVIOR_CONTINUE {
		t.Errorf("behavior = %v, want CONTINUE", result.Behavior)
	}
}

func TestPreIgnoresSaveOnlyCluster(t *testing.T) {
	ctx := context.Background()
	impl, _, _ := newHarness(t, testConfigStore())

	result, err := impl.Pre(ctx, request(t, testCluster(1)))
	if err != nil {
		t.Fatalf("a save-only cluster must not be blocked: %v", err)
	}
	if result.Behavior != reconciler.ReconcilerHooksResult_BEHAVIOR_CONTINUE {
		t.Errorf("behavior = %v, want CONTINUE", result.Behavior)
	}
}

// Kubernetes does not bump metadata.generation for label or annotation edits.
// A capture trigger keyed only on generation would therefore miss them
// entirely, and the change would sit uncaptured until an unrelated spec edit
// happened to flush it out.
func TestPostCapturesLabelChangeWithoutGenerationBump(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	first := testCluster(1)
	first.Labels = map[string]string{"team": "platform"}
	if _, err := impl.Post(ctx, request(t, first)); err != nil {
		t.Fatal(err)
	}
	if got := len(snapshotKeys(backend)); got != 1 {
		t.Fatalf("wrote %d snapshots, want 1", got)
	}
	firstChecksum := watermark(t, backend).Checksum

	// Same generation, different label — exactly what `kubectl label` produces.
	relabelled := testCluster(1)
	relabelled.Labels = map[string]string{"team": "platform", "tier": "gold"}
	if _, err := impl.Post(ctx, request(t, relabelled)); err != nil {
		t.Fatal(err)
	}

	if got := len(snapshotKeys(backend)); got != 2 {
		t.Errorf("a label change at the same generation was not captured (%d snapshots, want 2)", got)
	}
	state := watermark(t, backend)
	if state.Checksum == firstChecksum {
		t.Error("checksum did not change for a label change")
	}
	if state.Checksum == "" {
		t.Error("metadata fingerprint was not recorded")
	}
}

// Operator-owned labels must not trigger a capture: CloudNativePG rewrites
// cnpg.io/* labels during normal operation, and reacting to them would mean
// writing a snapshot on the operator's own bookkeeping.
func TestPostIgnoresOperatorOwnedMetadataChurn(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	first := testCluster(1)
	first.Labels = map[string]string{"team": "platform"}
	if _, err := impl.Post(ctx, request(t, first)); err != nil {
		t.Fatal(err)
	}
	writesAfterFirst := backend.Puts

	churned := testCluster(1)
	churned.Labels = map[string]string{"team": "platform", "cnpg.io/instanceRole": "primary"}
	churned.Annotations = map[string]string{"cnpg.io/operatorVersion": "1.30.1"}
	if _, err := impl.Post(ctx, request(t, churned)); err != nil {
		t.Fatal(err)
	}

	if backend.Puts != writesAfterFirst {
		t.Errorf("operator-owned metadata churn triggered a write (%d -> %d)",
			writesAfterFirst, backend.Puts)
	}
}

// The fingerprint must not make the fast path fire on every reconcile.
func TestPostStaysIdempotentWithMetadataFingerprint(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Labels = map[string]string{"team": "platform"}
	cluster.Annotations = map[string]string{"owner": "dba@example.com"}

	for i := 0; i < 5; i++ {
		if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
			t.Fatal(err)
		}
	}
	if backend.Puts != 2 {
		t.Errorf("issued %d writes across 5 reconciles, want 2", backend.Puts)
	}
}

// Enabling a capture group has to re-capture the clusters writing to that
// store. Nothing else would: the generation has not moved, the labels have not
// moved, and the fast path returns before Extract ever runs — so without the
// group set in the fingerprint the newly-enabled fields stay unwritten until an
// unrelated spec edit happens to flush them out.
func TestPostRecapturesWhenTheCaptureGroupSetChanges(t *testing.T) {
	ctx := context.Background()

	// One capture writes the snapshot and updates the latest.json pointer.
	const putsPerCapture = 2

	configStore := testConfigStore()
	impl, backend, kube := newHarness(t, configStore)

	cluster := testCluster(1)
	// Only the opt-in Identity group captures this.
	cluster.Spec.PostgresUID = 26

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != putsPerCapture {
		t.Fatalf("first capture issued %d writes, want %d", backend.Puts, putsPerCapture)
	}

	// Reconciling again changes nothing, as it should.
	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != putsPerCapture {
		t.Fatalf("an unchanged cluster issued %d writes, want %d", backend.Puts, putsPerCapture)
	}

	// Widen the store's capture set. Same cluster, same generation, same
	// labels — only the store configuration moved.
	var current chroniclev1.ConfigStore
	key := client.ObjectKey{Namespace: "default", Name: "config-store"}
	if err := kube.Get(ctx, key, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.CaptureGroups = append(snapshot.DefaultGroupNames(), "Identity")
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	if backend.Puts != 2*putsPerCapture {
		t.Fatalf("widening the capture set issued %d writes, want %d (a second capture)",
			backend.Puts, 2*putsPerCapture)
	}
}

// A plugin upgrade that adds a path to a group changes nothing on the Cluster:
// not its generation, not its labels. The capture rules digest is what notices,
// and the field has to be written out without waiting for an unrelated edit.
func TestPostRecapturesWhenAGroupGainsAPath(t *testing.T) {
	ctx := context.Background()
	impl, backend, kube := newHarness(t, testConfigStore())

	cluster := testCluster(1)
	cluster.Spec.PrimaryLease = &apiv1.PrimaryLeaseConfiguration{LeaseDurationSeconds: ptr.To(int32(30))}

	// Capture as a build whose Timings group did not yet carry primaryLease.
	timings := -1
	for i, group := range snapshot.Groups {
		if group.Name == "Timings" {
			timings = i
		}
	}
	current := snapshot.Groups[timings].Paths
	snapshot.Groups[timings].Paths = slices.DeleteFunc(slices.Clone(current),
		func(path string) bool { return path == "spec.primaryLease" })
	defer func() { snapshot.Groups[timings].Paths = current }()

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	status := OperatorImplementation{Client: kube, Resolver: storetest.Provider{Store: backend}, Settled: impl.Settled}
	published, err := status.SetStatusInCluster(ctx, statusRequest(t, cluster))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(backend.Objects["pg-source/chronicle/latest.json"]), "primaryLease") {
		t.Fatal("the older rules captured primaryLease; the test is not exercising an upgrade")
	}

	// Upgrade: the group now carries the path, in a fresh process.
	snapshot.Groups[timings].Paths = current
	impl.Settled = newSettledCache()
	puts := backend.Puts
	if _, err := impl.Post(ctx, request(t, withPublishedStatus(cluster, published.JsonStatus))); err != nil {
		t.Fatal(err)
	}
	if backend.Puts == puts {
		t.Fatal("the upgraded rules captured nothing, so primaryLease stays unrecorded")
	}
	if !strings.Contains(string(backend.Objects["pg-source/chronicle/latest.json"]), "primaryLease") {
		t.Error("the new snapshot does not carry primaryLease")
	}
}

// Rules that changed without changing this cluster's content still leave a
// snapshot recording the new rules, so the watermark lines up with them rather
// than missing on every reconcile after the next restart.
func TestPostRecordsNewRulesEvenWhenContentIsUnchanged(t *testing.T) {
	ctx := context.Background()
	impl, backend, _ := newHarness(t, testConfigStore())
	cluster := testCluster(1) // sets no primaryLease

	timings := -1
	for i, group := range snapshot.Groups {
		if group.Name == "Timings" {
			timings = i
		}
	}
	current := snapshot.Groups[timings].Paths
	snapshot.Groups[timings].Paths = slices.DeleteFunc(slices.Clone(current),
		func(path string) bool { return path == "spec.primaryLease" })
	defer func() { snapshot.Groups[timings].Paths = current }()

	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	before := watermark(t, backend)

	snapshot.Groups[timings].Paths = current
	impl.Settled = newSettledCache()
	if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
		t.Fatal(err)
	}
	after := watermark(t, backend)

	if after.Checksum != before.Checksum {
		t.Fatalf("content changed (%s -> %s); the test needs it unchanged", before.Checksum, after.Checksum)
	}
	if after.CaptureRules == before.CaptureRules {
		t.Error("the newest snapshot still records the old capture rules")
	}
}

// Narrowing must settle too, rather than rewriting on every pass.
func TestPostSettlesAfterTheCaptureGroupSetChanges(t *testing.T) {
	ctx := context.Background()

	configStore := testConfigStore()
	configStore.Spec.CaptureGroups = []string{"Gucs", "Storage"}
	impl, backend, _ := newHarness(t, configStore)

	cluster := testCluster(1)
	for i := 0; i < 4; i++ {
		if _, err := impl.Post(ctx, request(t, cluster)); err != nil {
			t.Fatal(err)
		}
	}
	if backend.Puts != 2 {
		t.Errorf("issued %d writes across 4 reconciles, want 2 (a single capture)", backend.Puts)
	}
}
