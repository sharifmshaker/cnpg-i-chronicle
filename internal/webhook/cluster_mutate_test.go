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

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

// --- harness -----------------------------------------------------------

func harness(t *testing.T, objects ...client.Object) (*ClusterRestorer, *storetest.Backend) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := chroniclev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	backend := storetest.NewBackend()
	return &ClusterRestorer{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Resolver: storetest.Provider{Store: backend},
	}, backend
}

func policyAndStore() []client.Object {
	return []client.Object{
		&chroniclev1.ConfigStore{
			ObjectMeta: metav1.ObjectMeta{Name: "config-store", Namespace: "default"},
			Spec:       chroniclev1.ConfigStoreSpec{Prefix: "chronicle"},
		},
		&chroniclev1.RestorePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "from-prod", Namespace: "default"},
			Spec: chroniclev1.RestorePolicySpec{
				Select: chroniclev1.RestoreSelect{Mode: chroniclev1.SelectLatest},
			},
		},
	}
}

const targetClusterJSON = `{
  "apiVersion": "postgresql.cnpg.io/v1",
  "kind": "Cluster",
  "metadata": {"name": "pg-restored", "namespace": "default"},
  "spec": {
    "instances": 1,
    "storage": {"size": "1Gi"},
    "plugins": [{"name": "chronicle.sharifmshaker.github.io", "parameters": {
      "restoreFromStore": "config-store",
      "restoreFromServer": "pg-prod",
      "restorePolicy": "from-prod"}}]
  }
}`

func admissionRequest(raw string) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "default",
		Object:    runtime.RawExtension{Raw: []byte(raw)},
	}}
}

// applyPatches replays the JSON patch the webhook returned, so assertions run
// against what the API server would actually persist.
func applyPatches(t *testing.T, original string, response admission.Response) map[string]any {
	t.Helper()
	if !response.Allowed {
		t.Fatalf("request denied: %v", response.Result)
	}
	encoded, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatchDecode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := patch.Apply([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pathutil.FromJSON(patched)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- tests -------------------------------------------------------------

func TestWebhookRestoresConfiguration(t *testing.T) {
	restorer, backend := harness(t, policyAndStore()...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 42, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC), `{
	  "instances": 3,
	  "storage": {"size": "100Gi"},
	  "postgresql": {"parameters": {"shared_buffers": "2GB", "pgaudit.log": "all"}}
	}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	cluster := applyPatches(t, targetClusterJSON, response)

	if got, _ := pathutil.Get(cluster, "spec.storage.size"); got != "100Gi" {
		t.Errorf("storage.size = %v, want 100Gi", got)
	}
	if got, _ := pathutil.Get(cluster, "spec.instances"); got.(json.Number).String() != "3" {
		t.Errorf("instances = %v, want 3", got)
	}
	if got, _ := pathutil.GetSegments(cluster,
		[]string{"spec", "postgresql", "parameters", "pgaudit.log"}); got != "all" {
		t.Errorf("pgaudit.log = %v, want all", got)
	}

	// Storage SHRANK, from the snapshot's 100Gi being applied over 1Gi at
	// CREATE. On update CNPG rejects that outright, which is the whole reason
	// restore runs in admission.
	record := restoreRecord(t, cluster)
	if record.Generation != 42 {
		t.Errorf("record.Generation = %d, want 42", record.Generation)
	}
	if record.Store != "config-store" || record.ServerName != "pg-prod" {
		t.Errorf("record does not identify the source: %+v", record)
	}
}

func restoreRecord(t *testing.T, cluster map[string]any) RestoreRecord {
	t.Helper()
	raw, found := pathutil.GetSegments(cluster,
		[]string{"metadata", "annotations", metadata.AnnotationRestoredFrom})
	if !found {
		t.Fatalf("the restored-from annotation was not set; annotations: %v",
			cluster["metadata"].(map[string]any)["annotations"])
	}
	var record RestoreRecord
	if err := json.Unmarshal([]byte(raw.(string)), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// The annotation key contains dots. If it were treated as a path it would
// become nested objects, and the Pre-reconcile guard would conclude the restore
// never ran and refuse the cluster forever.
func TestWebhookWritesASingleAnnotationKey(t *testing.T) {
	restorer, backend := harness(t, policyAndStore()...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 2}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	cluster := applyPatches(t, targetClusterJSON, response)

	annotations, ok := cluster["metadata"].(map[string]any)["annotations"].(map[string]any)
	if !ok {
		t.Fatal("no annotations were written")
	}
	if _, split := annotations["chronicle"]; split {
		t.Error("the annotation key was split into nested objects")
	}
	if _, whole := annotations[metadata.AnnotationRestoredFrom]; !whole {
		t.Errorf("annotation key not written whole; got %v", annotations)
	}
}

func TestWebhookIsIdempotent(t *testing.T) {
	restorer, backend := harness(t, policyAndStore()...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 2}`)

	first := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	cluster := applyPatches(t, targetClusterJSON, first)
	restored, err := json.Marshal(cluster)
	if err != nil {
		t.Fatal(err)
	}

	// A second admission pass over the already-restored object must not
	// re-resolve Latest and land a different snapshot.
	second := restorer.Handle(context.Background(), admissionRequest(string(restored)))
	if !second.Allowed {
		t.Fatalf("second pass denied: %v", second.Result)
	}
	if len(second.Patches) != 0 {
		t.Errorf("second pass produced %d patches, want 0", len(second.Patches))
	}
}

func TestWebhookDeniesMissingStore(t *testing.T) {
	restorer, _ := harness(t) // nothing exists
	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if response.Allowed {
		t.Fatal("a cluster naming a nonexistent ConfigStore was allowed")
	}
	if !strings.Contains(response.Result.Message, "config-store") {
		t.Errorf("message does not name the missing store: %s", response.Result.Message)
	}
}

// The store exists but the policy it names does not. Naming a policy is opting
// into rules, so a missing one is a mistake rather than "restore it plain".
func TestWebhookDeniesMissingPolicy(t *testing.T) {
	objects := policyAndStore()
	restorer, backend := harness(t, objects[0]) // the ConfigStore only
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 3}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if response.Allowed {
		t.Fatal("a cluster naming a nonexistent RestorePolicy was allowed")
	}
	if !strings.Contains(response.Result.Message, "from-prod") {
		t.Errorf("message does not name the missing policy: %s", response.Result.Message)
	}
}

func TestWebhookDeniesEmptyHistory(t *testing.T) {
	restorer, _ := harness(t, policyAndStore()...) // store exists, no snapshots
	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if response.Allowed {
		t.Fatal("a restore with no snapshots was allowed")
	}
}

// Saving to the same store and server name it restores from would interleave
// the new cluster's history with the source cluster's.
func TestWebhookDeniesSelfOverwritingConfiguration(t *testing.T) {
	restorer, backend := harness(t, policyAndStore()...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 2}`)

	colliding := `{
	  "apiVersion": "postgresql.cnpg.io/v1",
	  "kind": "Cluster",
	  "metadata": {"name": "pg-prod", "namespace": "default"},
	  "spec": {
	    "instances": 1,
	    "plugins": [{"name": "chronicle.sharifmshaker.github.io", "parameters": {
	      "restoreFromStore": "config-store", "restoreFromServer": "pg-prod",
	      "restorePolicy": "from-prod",
	      "saveToStore": "config-store", "saveToServer": "pg-prod"}}]
	  }
	}`

	response := restorer.Handle(context.Background(), admissionRequest(colliding))
	if response.Allowed {
		t.Fatal("a cluster that would overwrite the history it restored from was allowed")
	}
	if !strings.Contains(response.Result.Message, "interleave") {
		t.Errorf("unhelpful message: %s", response.Result.Message)
	}
}

// A different server name makes the same store safe to share.
func TestWebhookAllowsSharedStoreWithDistinctServerName(t *testing.T) {
	restorer, backend := harness(t, policyAndStore()...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 2}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if !response.Allowed {
		t.Fatalf("denied: %v", response.Result)
	}
}

func TestWebhookIgnoresClusterWithoutRestoreFrom(t *testing.T) {
	restorer, _ := harness(t, policyAndStore()...)
	saveOnly := `{
	  "apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
	  "metadata": {"name": "pg-a", "namespace": "default"},
	  "spec": {"plugins": [{"name": "chronicle.sharifmshaker.github.io", "parameters": {"saveToStore": "config-store"}}]}
	}`
	response := restorer.Handle(context.Background(), admissionRequest(saveOnly))
	if !response.Allowed {
		t.Fatalf("a save-only cluster was denied: %v", response.Result)
	}
	if len(response.Patches) != 0 {
		t.Errorf("a save-only cluster was patched: %v", response.Patches)
	}
}

func TestWebhookHonoursSkipRules(t *testing.T) {
	objects := policyAndStore()
	policy := objects[1].(*chroniclev1.RestorePolicy)
	policy.Spec.Skip = []chroniclev1.SkipRule{{Group: "Storage"}}

	restorer, backend := harness(t, objects...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{
	  "instances": 3, "storage": {"size": "100Gi"}
	}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	cluster := applyPatches(t, targetClusterJSON, response)

	if got, _ := pathutil.Get(cluster, "spec.storage.size"); got != "1Gi" {
		t.Errorf("storage.size = %v; the Storage skip should have kept the target's 1Gi", got)
	}
	if got, _ := pathutil.Get(cluster, "spec.instances"); got.(json.Number).String() != "3" {
		t.Errorf("instances = %v, want 3", got)
	}
}

// --- alignment with a recovery target ----------------------------------

func alignPolicyAndStore(action chroniclev1.UnresolvableAction) []client.Object {
	objects := policyAndStore()
	policy := objects[1].(*chroniclev1.RestorePolicy)
	policy.Spec.Select = chroniclev1.RestoreSelect{
		Mode:           chroniclev1.SelectAlignWithRecoveryTarget,
		OnUnresolvable: action,
	}
	return objects
}

func pitrCluster(recoveryTarget string) string {
	return `{
	  "apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
	  "metadata": {"name": "pg-restored", "namespace": "default"},
	  "spec": {
	    "instances": 1,
	    "storage": {"size": "1Gi"},
	    "bootstrap": {"recovery": ` + recoveryTarget + `},
	    "postgresql": {"parameters": {"shared_buffers": "999MB"}},
	    "plugins": [{"name": "chronicle.sharifmshaker.github.io", "parameters": {
	      "restoreFromStore": "config-store",
	      "restoreFromServer": "pg-prod",
	      "restorePolicy": "from-prod"}}]
	  }
	}`
}

// Recovering the data to an earlier moment must bring the configuration that
// was live then, not the newest one.
func TestWebhookAlignsWithTargetTime(t *testing.T) {
	restorer, backend := harness(t, alignPolicyAndStore("")...)
	early := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, early, `{"postgresql": {"parameters": {"shared_buffers": "128MB"}}}`)
	storetest.SeedSnapshot(t, backend, "pg-prod", 2, late, `{"postgresql": {"parameters": {"shared_buffers": "512MB"}}}`)

	raw := pitrCluster(`{"source": "origin", "recoveryTarget": {"targetTime": "2026-08-25T12:00:00Z"}}`)
	cluster := applyPatches(t, raw, restorer.Handle(context.Background(), admissionRequest(raw)))

	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); got != "128MB" {
		t.Errorf("shared_buffers = %v; want the 128MB in force at the recovery target", got)
	}
	record := restoreRecord(t, cluster)
	if !strings.Contains(record.AlignedWith, "targetTime") {
		t.Errorf("record does not explain the alignment: %q", record.AlignedWith)
	}
}

// The same cluster with no target recovers to the end of the archive, so the
// newest configuration is the matching one.
func TestWebhookAlignsUntargetedRecoveryToLatest(t *testing.T) {
	restorer, backend := harness(t, alignPolicyAndStore("")...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "128MB"}}}`)
	storetest.SeedSnapshot(t, backend, "pg-prod", 2, time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "512MB"}}}`)

	raw := pitrCluster(`{"source": "origin"}`)
	cluster := applyPatches(t, raw, restorer.Handle(context.Background(), admissionRequest(raw)))

	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); got != "512MB" {
		t.Errorf("shared_buffers = %v, want the newest 512MB", got)
	}
}

// Resolution through a real Backup resource, which is how backupID and
// bootstrap.recovery.backup are mapped to an instant.
func TestWebhookAlignsWithBackupResource(t *testing.T) {
	objects := alignPolicyAndStore("")
	objects = append(objects, &apiv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Status: apiv1.BackupStatus{
			BackupID: "20260825T110000",
			StoppedAt: &metav1.Time{
				Time: time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC),
			},
		},
	})

	restorer, backend := harness(t, objects...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "128MB"}}}`)
	storetest.SeedSnapshot(t, backend, "pg-prod", 2, time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "512MB"}}}`)

	for name, raw := range map[string]string{
		"by backup name": pitrCluster(`{"backup": {"name": "nightly"}}`),
		"by backupID":    pitrCluster(`{"source": "origin", "recoveryTarget": {"backupID": "20260825T110000"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			cluster := applyPatches(t, raw, restorer.Handle(context.Background(), admissionRequest(raw)))
			if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); got != "128MB" {
				t.Errorf("shared_buffers = %v; want the 128MB in force when the backup ended", got)
			}
		})
	}
}

// A target only PostgreSQL can interpret must block the cluster rather than
// silently pairing recovered data with the wrong configuration.
func TestWebhookDeniesUnresolvableTarget(t *testing.T) {
	restorer, backend := harness(t, alignPolicyAndStore("")...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC), `{"instances": 3}`)

	raw := pitrCluster(`{"source": "origin", "recoveryTarget": {"targetXID": "1234"}}`)
	response := restorer.Handle(context.Background(), admissionRequest(raw))
	if response.Allowed {
		t.Fatal("a cluster with an unresolvable recovery target was allowed")
	}
	if !strings.Contains(response.Result.Message, "targetXID") {
		t.Errorf("message does not name the problem: %s", response.Result.Message)
	}
}

func TestWebhookUnresolvableUseLatestOptOut(t *testing.T) {
	restorer, backend := harness(t, alignPolicyAndStore(chroniclev1.UnresolvableUseLatest)...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "128MB"}}}`)
	storetest.SeedSnapshot(t, backend, "pg-prod", 2, time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC),
		`{"postgresql": {"parameters": {"shared_buffers": "512MB"}}}`)

	raw := pitrCluster(`{"source": "origin", "recoveryTarget": {"targetXID": "1234"}}`)
	cluster := applyPatches(t, raw, restorer.Handle(context.Background(), admissionRequest(raw)))

	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); got != "512MB" {
		t.Errorf("shared_buffers = %v, want the newest 512MB", got)
	}
	// Even when falling back, the record must say the target was unresolvable.
	record := restoreRecord(t, cluster)
	if !strings.Contains(record.AlignedWith, "targetXID") {
		t.Errorf("record does not record the unresolvable target: %q", record.AlignedWith)
	}
}

// The policy's Ready condition is advisory; the webhook does not read it. So
// the same static checks run again at admission, and a policy the controller
// would mark unusable is refused for the same reason — before the object store
// is consulted, which is why no snapshot is seeded here.
func TestWebhookRefusesStaticallyInvalidPolicy(t *testing.T) {
	objects := policyAndStore()
	policy := objects[1].(*chroniclev1.RestorePolicy)
	policy.Spec.Transform = []chroniclev1.TransformRule{{Path: "spec.storage.sizze", Expression: "old"}}

	restorer, _ := harness(t, objects...)
	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if response.Allowed {
		t.Fatal("a policy naming a misspelled path was allowed")
	}
	for _, want := range []string{"from-prod", "spec.storage.sizze", "no such field"} {
		if !strings.Contains(response.Result.Message, want) {
			t.Errorf("message should mention %q; got: %s", want, response.Result.Message)
		}
	}
}

// A restore that applies nothing is a misconfiguration. Annotating the cluster
// as restored would leave an operator believing configuration was applied.
func TestWebhookDeniesRestoreThatChangesNothing(t *testing.T) {
	objects := policyAndStore()
	policy := objects[1].(*chroniclev1.RestorePolicy)
	policy.Spec.Skip = []chroniclev1.SkipRule{{Path: "spec"}, {Path: "metadata"}}

	restorer, backend := harness(t, objects...)
	storetest.SeedSnapshot(t, backend, "pg-prod", 1, time.Now().UTC(), `{"instances": 3}`)

	response := restorer.Handle(context.Background(), admissionRequest(targetClusterJSON))
	if response.Allowed {
		t.Fatal("a restore that changes nothing was allowed")
	}
	for _, want := range []string{"would change nothing", "from-prod"} {
		if !strings.Contains(response.Result.Message, want) {
			t.Errorf("message should mention %q; got: %s", want, response.Result.Message)
		}
	}
}
