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

package restore

import (
	"encoding/json"
	"strings"
	"testing"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

func sourceSnapshot(t *testing.T) *snapshot.Snapshot {
	t.Helper()
	spec, err := pathutil.FromJSON([]byte(`{
	  "instances": 3,
	  "imageName": "ghcr.io/cloudnative-pg/postgresql:18",
	  "storage": {"size": "100Gi", "storageClass": "fast"},
	  "resources": {"requests": {"cpu": "2", "memory": "8Gi"}},
	  "postgresql": {
	    "parameters": {"shared_buffers": "2GB", "work_mem": "64MB", "max_connections": "500"},
	    "pg_hba": ["host all all 10.0.0.0/8 md5"]
	  },
	  "affinity": {"topologyKey": "topology.kubernetes.io/zone"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion,
		Kind:       snapshot.Kind,
		Generation: 42,
		Content: snapshot.Content{
			Spec:        spec,
			Labels:      map[string]string{"team": "platform"},
			Annotations: map[string]string{"owner": "dba@example.com"},
		},
	}
}

// The target is what an operator would actually write for a restored cluster:
// small, with a couple of deliberate local choices.
func targetCluster(t *testing.T) map[string]any {
	t.Helper()
	cluster, err := pathutil.FromJSON([]byte(`{
	  "apiVersion": "postgresql.cnpg.io/v1",
	  "kind": "Cluster",
	  "metadata": {"name": "pg-restored", "namespace": "default", "labels": {"env": "staging"}},
	  "spec": {
	    "instances": 1,
	    "storage": {"size": "1Gi"},
	    "bootstrap": {"recovery": {"source": "origin"}},
	    "plugins": [{"name": "chronicle.sharifmshaker.github.io", "parameters": {"restoreFrom": "from-prod"}}]
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

func mustPlan(t *testing.T, rules []chroniclev1.SkipRule) *Plan {
	t.Helper()
	skips, err := NewSkipSet(rules)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(sourceSnapshot(t), skips, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPlanRestoresConfiguration(t *testing.T) {
	plan := mustPlan(t, nil)
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{
		"spec.storage.size":                         "100Gi",
		"spec.storage.storageClass":                 "fast",
		"spec.resources.requests.memory":            "8Gi",
		"spec.postgresql.parameters.shared_buffers": "2GB",
		"spec.imageName":                            "ghcr.io/cloudnative-pg/postgresql:18",
		"metadata.labels.team":                      "platform",
		"metadata.annotations.owner":                "dba@example.com",
	} {
		got, found := pathutil.Get(cluster, path)
		if !found {
			t.Errorf("%s was not restored", path)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}

	// Running at CREATE means storage may shrink as well as grow; an update
	// would be rejected for this.
	if size, _ := pathutil.Get(cluster, "spec.storage.size"); size != "100Gi" {
		t.Errorf("storage.size = %v, want 100Gi", size)
	}
}

// The target's own identity and restore intent must survive untouched.
func TestPlanNeverTouchesIdentityOrBootstrap(t *testing.T) {
	plan := mustPlan(t, nil)
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	if name, _ := pathutil.Get(cluster, "metadata.name"); name != "pg-restored" {
		t.Errorf("metadata.name was overwritten: %v", name)
	}
	if source, _ := pathutil.Get(cluster, "spec.bootstrap.recovery.source"); source != "origin" {
		t.Errorf("bootstrap was overwritten: %v", source)
	}
	if _, found := pathutil.Get(cluster, "spec.plugins"); !found {
		t.Error("the target's own plugin configuration was removed")
	}

	// The user's unrelated label survives; the restored one is added.
	if env, _ := pathutil.Get(cluster, "metadata.labels.env"); env != "staging" {
		t.Errorf("the target's own label was lost: %v", env)
	}
}

// A snapshot may come from an older build or be hand-edited. The denylist is
// re-checked at restore time rather than trusted from capture time.
func TestPlanRefusesArchivingConfigurationInsideASnapshot(t *testing.T) {
	tampered := sourceSnapshot(t)
	tampered.Spec["plugins"] = []any{map[string]any{
		"name":       "barman-cloud.cloudnative-pg.io",
		"parameters": map[string]any{"barmanObjectName": "objectstore-prod"},
	}}
	tampered.Spec["backup"] = map[string]any{"retentionPolicy": "30d"}
	tampered.Spec["externalClusters"] = []any{map[string]any{"name": "origin"}}

	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(tampered, skips, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range plan.Changes {
		for _, forbidden := range []string{"spec.plugins", "spec.backup", "spec.externalClusters"} {
			if change.Path == forbidden || strings.HasPrefix(change.Path, forbidden+".") {
				t.Errorf("plan would restore %q from a tampered snapshot", change.Path)
			}
		}
	}

	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}
	if _, found := pathutil.Get(cluster, "spec.backup"); found {
		t.Error("spec.backup leaked into the restored cluster")
	}
	plugins, _ := pathutil.Get(cluster, "spec.plugins")
	if list, ok := plugins.([]any); ok && len(list) == 1 {
		entry := list[0].(map[string]any)
		if entry["name"] != "chronicle.sharifmshaker.github.io" {
			t.Errorf("spec.plugins was replaced by the snapshot's: %v", entry["name"])
		}
	}
}

func TestSkipByGroup(t *testing.T) {
	plan := mustPlan(t, []chroniclev1.SkipRule{{Group: "Storage"}, {Group: "Resources"}})
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	if size, _ := pathutil.Get(cluster, "spec.storage.size"); size != "1Gi" {
		t.Errorf("storage.size = %v; skipping Storage should have left the target's 1Gi", size)
	}
	if _, found := pathutil.Get(cluster, "spec.resources"); found {
		t.Error("resources were restored despite skipping the Resources group")
	}
	// Unrelated groups still apply.
	if buffers, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); buffers != "2GB" {
		t.Errorf("shared_buffers = %v, want 2GB", buffers)
	}
}

func TestSkipSinglePathAndGlob(t *testing.T) {
	plan := mustPlan(t, []chroniclev1.SkipRule{
		{Path: "spec.postgresql.parameters.work_mem"},
	})
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}
	if _, found := pathutil.Get(cluster, "spec.postgresql.parameters.work_mem"); found {
		t.Error("work_mem was restored despite being skipped")
	}
	if buffers, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); buffers != "2GB" {
		t.Error("skipping one GUC removed its siblings")
	}

	// The glob form skips every GUC.
	globPlan := mustPlan(t, []chroniclev1.SkipRule{{Path: "spec.postgresql.parameters.*"}})
	globCluster := targetCluster(t)
	if err := globPlan.Apply(globCluster); err != nil {
		t.Fatal(err)
	}
	if _, found := pathutil.Get(globCluster, "spec.postgresql.parameters"); found {
		t.Error("parameters were restored despite the glob skip")
	}
	// pg_hba is a sibling of parameters, not a child; it must survive.
	if _, found := pathutil.Get(globCluster, "spec.postgresql.pg_hba"); !found {
		t.Error("the parameters glob wrongly skipped pg_hba")
	}
}

// A bare path is a subtree skip: writing "spec.storage" and expecting
// "spec.storage.size" to survive is the only sensible reading.
func TestSkipBarePathCoversSubtree(t *testing.T) {
	plan := mustPlan(t, []chroniclev1.SkipRule{{Path: "spec.storage"}})
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}
	if size, _ := pathutil.Get(cluster, "spec.storage.size"); size != "1Gi" {
		t.Errorf("storage.size = %v, want the target's 1Gi", size)
	}
}

func TestSkipMetadataGroupCoversLabelsAndAnnotations(t *testing.T) {
	plan := mustPlan(t, []chroniclev1.SkipRule{{Group: "Metadata"}})
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}
	if _, found := pathutil.Get(cluster, "metadata.labels.team"); found {
		t.Error("labels were restored despite skipping the Metadata group")
	}
	if _, found := pathutil.Get(cluster, "metadata.annotations.owner"); found {
		t.Error("annotations were restored despite skipping the Metadata group")
	}
	if env, _ := pathutil.Get(cluster, "metadata.labels.env"); env != "staging" {
		t.Error("the target's own label was disturbed")
	}
}

func TestSkipSetRejectsBadRules(t *testing.T) {
	if _, err := NewSkipSet([]chroniclev1.SkipRule{{Group: "NoSuchGroup"}}); err == nil {
		t.Error("an unknown group should be rejected")
	}
	if _, err := NewSkipSet([]chroniclev1.SkipRule{{Group: "Storage", Path: "spec.storage"}}); err == nil {
		t.Error("a rule setting both group and path should be rejected")
	}
	if _, err := NewSkipSet([]chroniclev1.SkipRule{{}}); err == nil {
		t.Error("an empty rule should be rejected")
	}
}

func TestPlanReportsExclusions(t *testing.T) {
	plan := mustPlan(t, []chroniclev1.SkipRule{{Group: "Storage"}})
	var found bool
	for _, exclusion := range plan.Exclusions {
		if exclusion.Path == "spec.storage.size" {
			found = true
			if !strings.Contains(exclusion.Reason, "Storage") {
				t.Errorf("exclusion reason %q does not name the rule", exclusion.Reason)
			}
		}
	}
	if !found {
		t.Errorf("spec.storage.size missing from exclusions: %+v", plan.Exclusions)
	}
}

// PostgreSQL namespaced GUCs and label keys contain dots. If the merge treated
// a path as splittable all the way down, "pgaudit.log" would arrive as a nested
// object {"pgaudit": {"log": ...}} — which is not a GUC, and which CloudNativePG
// would reject or silently ignore.
func TestPlanPreservesDottedKeys(t *testing.T) {
	spec, err := pathutil.FromJSON([]byte(`{
	  "postgresql": {"parameters": {"pgaudit.log": "all", "shared_buffers": "2GB"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion,
		Kind:       snapshot.Kind,
		Content: snapshot.Content{
			Spec:        spec,
			Labels:      map[string]string{"example.com/team": "platform"},
			Annotations: map[string]string{"example.com/owner": "dba"},
		},
	}

	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(snap, skips, nil)
	if err != nil {
		t.Fatal(err)
	}
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	parameters := cluster["spec"].(map[string]any)["postgresql"].(map[string]any)["parameters"].(map[string]any)
	if got, ok := parameters["pgaudit.log"]; !ok || got != "all" {
		t.Errorf(`parameters["pgaudit.log"] = %v, %v; got map %#v`, got, ok, parameters)
	}
	if _, split := parameters["pgaudit"]; split {
		t.Error(`the GUC "pgaudit.log" was split into a nested object`)
	}

	labels := cluster["metadata"].(map[string]any)["labels"].(map[string]any)
	if got := labels["example.com/team"]; got != "platform" {
		t.Errorf(`labels["example.com/team"] = %v; got map %#v`, got, labels)
	}
	if _, split := labels["example"]; split {
		t.Error("the dotted label key was split")
	}

	annotations := cluster["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := annotations["example.com/owner"]; got != "dba" {
		t.Errorf(`annotations["example.com/owner"] = %v; got map %#v`, got, annotations)
	}
}

// Skip rules are matched as strings, so they still address dotted keys.
func TestSkipMatchesDottedKeys(t *testing.T) {
	spec, err := pathutil.FromJSON([]byte(`{
	  "postgresql": {"parameters": {"pgaudit.log": "all", "shared_buffers": "2GB"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion, Kind: snapshot.Kind,
		Content: snapshot.Content{Spec: spec},
	}

	skips, err := NewSkipSet([]chroniclev1.SkipRule{
		{Path: "spec.postgresql.parameters.pgaudit.log"},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(snap, skips, nil)
	if err != nil {
		t.Fatal(err)
	}
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	parameters := cluster["spec"].(map[string]any)["postgresql"].(map[string]any)["parameters"].(map[string]any)
	if _, present := parameters["pgaudit.log"]; present {
		t.Error("the dotted GUC was restored despite an exact skip rule")
	}
	if parameters["shared_buffers"] != "2GB" {
		t.Error("skipping the dotted GUC removed a sibling")
	}
}

// A policy decides which labels and annotations travel. Each rule removes a
// set of keys and except exempts keys from its own rule, which covers "none",
// "only these", and "none with this prefix" without the plugin guessing which
// tool owns what.
func TestSkipLabelsAndAnnotationsByKey(t *testing.T) {
	snap := &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion, Kind: snapshot.Kind,
		Content: snapshot.Content{
			Labels: map[string]string{
				"team":                       "platform",
				"app.kubernetes.io/name":     "billing",
				"app.kubernetes.io/instance": "billing-prod",
			},
			Annotations: map[string]string{
				"example.com/owner":              "dba",
				"example.com/cost-centre":        "42",
				"argocd.argoproj.io/tracking-id": "prod:postgresql.cnpg.io/Cluster:db/pg",
			},
		},
	}
	skips, err := NewSkipSet([]chroniclev1.SkipRule{
		// Every app.kubernetes.io label but the name.
		{Path: "metadata.labels", KeyPrefix: "app.kubernetes.io/", Except: []string{"app.kubernetes.io/name"}},
		// Only the owner annotation.
		{Path: "metadata.annotations", Except: []string{"example.com/owner"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(snap, skips, nil)
	if err != nil {
		t.Fatal(err)
	}
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	labels := cluster["metadata"].(map[string]any)["labels"].(map[string]any)
	annotations := cluster["metadata"].(map[string]any)["annotations"].(map[string]any)
	for key, want := range map[string]bool{"team": true, "app.kubernetes.io/name": true, "app.kubernetes.io/instance": false, "env": true} {
		if _, got := labels[key]; got != want {
			t.Errorf("label %q restored = %v, want %v; labels %v", key, got, want, labels)
		}
	}
	for key, want := range map[string]bool{"example.com/owner": true, "example.com/cost-centre": false, "argocd.argoproj.io/tracking-id": false} {
		if _, got := annotations[key]; got != want {
			t.Errorf("annotation %q restored = %v, want %v; annotations %v", key, got, want, annotations)
		}
	}
}

// A key inside a map is one key, dots and all. A path rule naming one must not
// also swallow every key that happens to start with the same text.
func TestSkipPathInsideAMapMatchesTheWholeKey(t *testing.T) {
	skips, err := NewSkipSet([]chroniclev1.SkipRule{{Path: "metadata.annotations.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if skipped, _ := skips.Skips("metadata.annotations.example.com/owner"); skipped {
		t.Error(`a rule for the annotation "example" skipped "example.com/owner"`)
	}
	if skipped, _ := skips.Skips("metadata.annotations.example"); !skipped {
		t.Error(`a rule for the annotation "example" did not skip it`)
	}
}

func TestKeyedSkipRulesNeedAMap(t *testing.T) {
	for _, rule := range []chroniclev1.SkipRule{
		{Path: "spec.storage.size", KeyPrefix: "x"},
		{Path: "spec.postgresql", Except: []string{"x"}},
		{Group: "Metadata", KeyPrefix: "x"},
	} {
		if _, err := NewSkipSet([]chroniclev1.SkipRule{rule}); err == nil {
			t.Errorf("%+v should be rejected: keyPrefix and except apply only to a map", rule)
		}
	}
}

// Metadata keys the capture filter refuses are refused on restore too, because
// a snapshot may have been written by another build or edited by hand.
func TestRestoreRefusesDeniedMetadataKeysFromASnapshot(t *testing.T) {
	snap := &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion, Kind: snapshot.Kind,
		Content: snapshot.Content{
			Labels:      map[string]string{"team": "platform", "cnpg.io/cluster": "pg-prod"},
			Annotations: map[string]string{"chronicle.sharifmshaker.github.io/restored-from": "{}"},
		},
	}
	plan, err := BuildPlan(snap, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range plan.Changes {
		if strings.Contains(change.Path, "cnpg.io/") || strings.Contains(change.Path, "chronicle.sharifmshaker.github.io/") {
			t.Errorf("the plan would restore %s", change.Path)
		}
	}
	if len(plan.Exclusions) != 2 {
		t.Errorf("exclusions = %+v, want both denied keys recorded", plan.Exclusions)
	}
}

// --- transforms ---------------------------------------------------------

func mustTransforms(t *testing.T, rules ...chroniclev1.TransformRule) *TransformSet {
	t.Helper()
	set, err := NewTransformSet(rules)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Inherit production's shape without inheriting its size.
func TestTransformResizesOnTheWayIn(t *testing.T) {
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.storage.size", Expression: "old.divide(10)"},
		chroniclev1.TransformRule{Path: "spec.resources.requests.memory", Expression: "old.divide(4)"},
		chroniclev1.TransformRule{Path: "spec.instances", Expression: "old > 1 ? 1 : old"},
		chroniclev1.TransformRule{
			Path: "spec.postgresql.parameters.shared_buffers", Expression: "pgScale(old, 0.5)",
		},
	)

	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(sourceSnapshot(t), skips, transforms)
	if err != nil {
		t.Fatal(err)
	}
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	// Source: 100Gi storage, 8Gi memory, 3 instances, 2GB shared_buffers.
	for path, want := range map[string]string{
		"spec.storage.size":              "10Gi",
		"spec.resources.requests.memory": "2Gi",
	} {
		got, _ := pathutil.Get(cluster, path)
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if got, _ := pathutil.Get(cluster, "spec.instances"); got.(json.Number).String() != "1" {
		t.Errorf("instances = %v, want 1", got)
	}
	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.shared_buffers"); got != "1GB" {
		t.Errorf("shared_buffers = %v, want 1GB", got)
	}

	// Untransformed values come across untouched.
	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.max_connections"); got != "500" {
		t.Errorf("max_connections = %v, want the source's 500", got)
	}

	transformed := plan.TransformedPaths()
	if len(transformed) != 4 {
		t.Errorf("TransformedPaths = %v, want all four", transformed)
	}
}

// The type `old` has comes from the path, and the result goes back in the
// type the captured value had. Exercised through BuildPlan because that is
// where the path is known: a GUC must come back a string or the API server
// rejects the Cluster, and a CPU request of "2" must still be a quantity.
func TestTransformTypesFollowThePath(t *testing.T) {
	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.postgresql.parameters.max_connections", Expression: "old * 2"},
		chroniclev1.TransformRule{Path: "spec.resources.requests.cpu", Expression: "old.multiply(2)"},
	)

	plan, err := BuildPlan(sourceSnapshot(t), skips, transforms)
	if err != nil {
		t.Fatal(err)
	}
	cluster := targetCluster(t)
	if err := plan.Apply(cluster); err != nil {
		t.Fatal(err)
	}

	// Source: max_connections "500", cpu "2".
	if got, _ := pathutil.Get(cluster, "spec.postgresql.parameters.max_connections"); got != "1000" {
		t.Errorf("max_connections = %#v, want the string \"1000\"", got)
	}
	if got, _ := pathutil.Get(cluster, "spec.resources.requests.cpu"); got != "4" {
		t.Errorf("cpu = %#v, want the quantity \"4\"", got)
	}
}

// A transform whose path is mistyped would silently leave production's value in
// place — a cluster restored at full size because the expression meant to
// shrink it never ran.
func TestTransformOnUnknownPathIsAnError(t *testing.T) {
	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.storage.sizze", Expression: "old.divide(2)"})

	_, err = BuildPlan(sourceSnapshot(t), skips, transforms)
	if err == nil {
		t.Fatal("a transform matching nothing should fail the restore")
	}
	if !strings.Contains(err.Error(), "captured no such value") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A transform aimed one level too high is the dangerous kind of inert: the
// values beneath it are restored anyway, untransformed, so the cluster comes up
// at production's size with nothing having failed. The path is real and
// resolves against CloudNativePG's types, so no static check can catch it — it
// has to be caught against the snapshot, and the message has to say which
// mistake it was.
func TestTransformOnAnObjectIsAnError(t *testing.T) {
	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.storage", Expression: "old"})

	_, err = BuildPlan(sourceSnapshot(t), skips, transforms)
	if err == nil {
		t.Fatal("transforming an object should fail rather than silently restore its children")
	}
	for _, want := range []string{"names an object", "2 value(s) beneath"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

// Skipping and transforming the same path is contradictory: the skip wins, so
// the transform could never run.
func TestTransformOnSkippedPathIsAnError(t *testing.T) {
	skips, err := NewSkipSet([]chroniclev1.SkipRule{{Group: "Storage"}})
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.storage.size", Expression: "old.divide(2)"})

	_, err = BuildPlan(sourceSnapshot(t), skips, transforms)
	if err == nil {
		t.Fatal("transforming a skipped path should fail")
	}
	if !strings.Contains(err.Error(), "both transformed and skipped") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// onMissing: Ignore is for a path that some histories legitimately lack —
// a field CloudNativePG added, or a capture group that grew, after an old
// snapshot was written. The rest of the restore must still happen.
func TestTransformToleratesMissingPathWhenAsked(t *testing.T) {
	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t, chroniclev1.TransformRule{
		Path:       "spec.primaryLease.leaseDurationSeconds",
		Expression: "old",
		OnMissing:  chroniclev1.MissingIgnore,
	})

	plan, err := BuildPlan(sourceSnapshot(t), skips, transforms)
	if err != nil {
		t.Fatalf("an absent path marked Ignore should not fail the restore: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Error("the rest of the snapshot should still restore")
	}
}

// Ignore covers absence, nothing else. A path whose values are present but sit
// beneath it still fails, because those values would restore untransformed —
// which is the failure the check exists for.
func TestTransformIgnoreDoesNotExcuseAnObjectPath(t *testing.T) {
	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t, chroniclev1.TransformRule{
		Path: "spec.storage", Expression: "old", OnMissing: chroniclev1.MissingIgnore,
	})

	if _, err := BuildPlan(sourceSnapshot(t), skips, transforms); err == nil {
		t.Fatal("Ignore must not suppress a transform aimed at an object")
	} else if !strings.Contains(err.Error(), "names an object") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// And it does not excuse a contradiction either: a skipped path is never
// restored, so the transform could not run whatever the history held.
func TestTransformIgnoreDoesNotExcuseASkippedPath(t *testing.T) {
	skips, err := NewSkipSet([]chroniclev1.SkipRule{{Path: "spec.storage.size"}})
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t, chroniclev1.TransformRule{
		Path: "spec.storage.size", Expression: "old.divide(2)", OnMissing: chroniclev1.MissingIgnore,
	})

	if _, err := BuildPlan(sourceSnapshot(t), skips, transforms); err == nil {
		t.Fatal("Ignore must not suppress a skipped-and-transformed contradiction")
	} else if !strings.Contains(err.Error(), "both transformed and skipped") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestTransformSetRejectsBadRules(t *testing.T) {
	if _, err := NewTransformSet([]chroniclev1.TransformRule{
		{Path: "spec.storage.size", Expression: "old.divide("},
	}); err == nil {
		t.Error("a syntax error should be rejected at compile time")
	}
	if _, err := NewTransformSet([]chroniclev1.TransformRule{
		{Path: "spec.storage.size", Expression: "old.divide(2)"},
		{Path: "spec.storage.size", Expression: "old.multiply(2)"},
	}); err == nil {
		t.Error("two rules for one path should be rejected")
	}
	if _, err := NewTransformSet([]chroniclev1.TransformRule{
		{Path: "", Expression: "old"},
	}); err == nil {
		t.Error("a rule with no path should be rejected")
	}
}

// A type mismatch cannot be caught at compile time, so it must fail the restore
// rather than produce a wrong value.
func TestTransformTypeMismatchFailsTheRestore(t *testing.T) {
	snap := sourceSnapshot(t)
	parameters := snap.Spec["postgresql"].(map[string]any)["parameters"].(map[string]any)
	parameters["ssl"] = "on"

	skips, err := NewSkipSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	transforms := mustTransforms(t,
		chroniclev1.TransformRule{Path: "spec.postgresql.parameters.ssl", Expression: "old.multiply(2)"})

	if _, err := BuildPlan(snap, skips, transforms); err == nil {
		t.Fatal(`multiplying the GUC "on" should fail the restore`)
	}
}

// The denylist is the safety property this plugin rests on: restoring
// spec.plugins, spec.backup, spec.externalClusters, spec.bootstrap or
// spec.replica would point a new cluster at the source cluster's WAL archive,
// or at its recovery, or make it a replica of something it is not.
//
// Driven from snapshot.DeniedPaths rather than a list written out here, so
// adding an entry there cannot silently go untested. A snapshot is an input
// from an object store — it may have been written by an older build or by
// hand — so this plants each denied path in one and checks it reaches neither
// the plan nor the cluster.
func TestNoDeniedPathSurvivesIntoARestore(t *testing.T) {
	for _, denied := range snapshot.DeniedPaths {
		if !strings.HasPrefix(denied, "spec.") {
			// metadata.* and status cannot be represented in a snapshot's
			// content, which only carries spec, labels and annotations.
			continue
		}
		t.Run(denied, func(t *testing.T) {
			field := strings.TrimPrefix(denied, "spec.")
			tampered := &snapshot.Snapshot{
				Groups: snapshot.DefaultGroupNames(),
				Content: snapshot.Content{Spec: map[string]any{
					// Something legitimate, so the plan is not empty.
					"instances": float64(3),
					// And the forbidden path, as a hand-edited snapshot would.
					field: map[string]any{"injected": "value"},
				}},
			}

			skips, err := NewSkipSet(nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := BuildPlan(tampered, skips, nil)
			if err != nil {
				t.Fatal(err)
			}

			for _, change := range plan.Changes {
				if change.Path == denied || strings.HasPrefix(change.Path, denied+".") {
					t.Errorf("the plan would restore %q from a tampered snapshot", change.Path)
				}
			}

			cluster := map[string]any{"spec": map[string]any{}}
			if err := plan.Apply(cluster); err != nil {
				t.Fatal(err)
			}
			if _, found := pathutil.Get(cluster, denied); found {
				t.Errorf("%s leaked into the restored cluster", denied)
			}
		})
	}
}
