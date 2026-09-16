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

package snapshot

import (
	"strings"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

// realisticCluster is shaped like a production cluster: archiving configured
// through the barman plugin, a recovery external cluster, real affinity,
// walStorage and resource limits.
func realisticCluster() *apiv1.Cluster {
	return &apiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pg-source",
			Namespace:  "default",
			UID:        "11111111-2222-3333-4444-555555555555",
			Generation: 42,
			Labels: map[string]string{
				"team":                   "platform",
				"cnpg.io/cluster":        "pg-source",
				"app.kubernetes.io/name": "cloudnative-pg",
			},
			Annotations: map[string]string{
				"owner":                   "dba@example.com",
				"cnpg.io/operatorVersion": "1.30.0",
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
		},
		Spec: apiv1.ClusterSpec{
			Instances: 3,
			ImageName: "ghcr.io/cloudnative-pg/postgresql:18",
			PostgresConfiguration: apiv1.PostgresConfiguration{
				Parameters: map[string]string{
					"shared_buffers": "128MB",
					"work_mem":       "4MB",
					// Operator-managed; must not survive capture.
					"archive_mode":    "on",
					"archive_command": "/controller/manager wal-archive %p",
					"ssl":             "on",
				},
				PgHBA:               []string{"host all all 10.0.0.0/8 md5"},
				AdditionalLibraries: []string{"pgaudit", "pg_stat_statements", "custom_lib"},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			StorageConfiguration: apiv1.StorageConfiguration{
				Size:         "10Gi",
				StorageClass: ptr.To("csi-hostpath-sc"),
			},
			WalStorage: &apiv1.StorageConfiguration{Size: "2Gi"},
			Affinity: apiv1.AffinityConfiguration{
				EnablePodAntiAffinity: ptr.To(true),
				TopologyKey:           "topology.kubernetes.io/zone",
				PodAntiAffinityType:   "required",
			},

			// Everything below is on the hard denylist. A restored cluster that
			// inherited any of it would archive WAL into the source cluster's
			// server name.
			Plugins: []apiv1.PluginConfiguration{{
				Name:          "barman-cloud.cloudnative-pg.io",
				IsWALArchiver: ptr.To(true),
				Parameters:    map[string]string{"barmanObjectName": "backup-store"},
			}},
			Backup: &apiv1.BackupConfiguration{RetentionPolicy: "30d"},
			ExternalClusters: []apiv1.ExternalCluster{{
				Name: "source",
				PluginConfiguration: &apiv1.PluginConfiguration{
					Name:       "barman-cloud.cloudnative-pg.io",
					Parameters: map[string]string{"barmanObjectName": "backup-store"},
				},
			}},
			Bootstrap: &apiv1.BootstrapConfiguration{
				Recovery: &apiv1.BootstrapRecovery{Source: "source"},
			},
		},
	}
}

// TestExtractNeverCapturesArchivingConfiguration is the single most important
// test in the project. If .spec.plugins, .spec.backup or .spec.externalClusters
// ever leak into a snapshot, restoring it points a second live cluster at the
// first one's WAL archive.
func TestExtractNeverCapturesArchivingConfiguration(t *testing.T) {
	got, err := Extract(realisticCluster(), ExtractOptions{PluginVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := Canonical(got.Content)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)

	for _, forbidden := range []string{
		"plugins", "barmanObjectName", "barman-cloud.cloudnative-pg.io",
		"externalClusters", "bootstrap", "retentionPolicy", "backups",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Errorf("snapshot leaked %q:\n%s", forbidden, serialized)
		}
	}

	for _, denied := range DeniedPaths {
		if _, found := pathutil.Get(map[string]any{"spec": got.Spec}, denied); found {
			t.Errorf("denied path %q present in snapshot", denied)
		}
	}
}

// Even if someone adds a denied path to a capture group, extraction must fail
// loudly rather than quietly widening what gets carried across clusters.
func TestExtractRejectsGroupNamingDeniedPath(t *testing.T) {
	Groups = append(Groups, Group{Name: "Rogue", Paths: []string{"spec.backup"}, DefaultEnabled: true})
	defer func() {
		Groups = Groups[:len(Groups)-1]
	}()

	_, err := Extract(realisticCluster(), ExtractOptions{Groups: []string{"Rogue"}})
	if err == nil || !strings.Contains(err.Error(), "denied path") {
		t.Fatalf("err = %v; want a denied-path failure", err)
	}
}

func TestExtractDropsOperatorManagedGUCs(t *testing.T) {
	got, err := Extract(realisticCluster(), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	parameters, found := pathutil.Get(map[string]any{"spec": got.Spec}, "spec.postgresql.parameters")
	if !found {
		t.Fatal("expected postgresql.parameters to be captured")
	}
	asMap := parameters.(map[string]any)

	for _, fixed := range []string{"archive_mode", "archive_command", "ssl"} {
		if _, present := asMap[fixed]; present {
			t.Errorf("operator-managed GUC %q was captured", fixed)
		}
	}
	for _, wanted := range []string{"shared_buffers", "work_mem"} {
		if _, present := asMap[wanted]; !present {
			t.Errorf("user GUC %q was dropped", wanted)
		}
	}
}

func TestExtractDropsManagedSharedPreloadLibraries(t *testing.T) {
	got, err := Extract(realisticCluster(), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	libraries, found := pathutil.Get(map[string]any{"spec": got.Spec}, "spec.postgresql.shared_preload_libraries")
	if !found {
		t.Fatal("expected shared_preload_libraries to be captured")
	}

	var names []string
	for _, entry := range libraries.([]any) {
		names = append(names, entry.(string))
	}
	// CNPG re-derives pgaudit and pg_stat_statements from the presence of their
	// namespaced GUCs, so carrying them explicitly is at best redundant.
	for _, managed := range []string{"pgaudit", "pg_stat_statements"} {
		for _, name := range names {
			if name == managed {
				t.Errorf("operator-managed library %q was captured", managed)
			}
		}
	}
	if len(names) != 1 || names[0] != "custom_lib" {
		t.Errorf("libraries = %v, want [custom_lib]", names)
	}
}

func TestExtractFiltersOperatorOwnedMetadata(t *testing.T) {
	got, err := Extract(realisticCluster(), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, present := got.Labels["cnpg.io/cluster"]; present {
		t.Error("operator-owned label was captured")
	}
	// Whether another tool's keys should travel depends on how the target is
	// managed, so they are captured and left to a policy's skip rules.
	if got.Labels["app.kubernetes.io/name"] != "cloudnative-pg" {
		t.Errorf("a label owned by neither CloudNativePG nor this plugin was dropped: %v", got.Labels)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("user label lost: %v", got.Labels)
	}
	if _, present := got.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("kubectl bookkeeping annotation was captured")
	}
	if got.Annotations["owner"] != "dba@example.com" {
		t.Errorf("user annotation lost: %v", got.Annotations)
	}
}

func TestExtractCapturesTheThingsWeCareAbout(t *testing.T) {
	got, err := Extract(realisticCluster(), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	wrapped := map[string]any{"spec": got.Spec}
	for path, want := range map[string]string{
		"spec.storage.size":         "10Gi",
		"spec.storage.storageClass": "csi-hostpath-sc",
		"spec.walStorage.size":      "2Gi",
		"spec.imageName":            "ghcr.io/cloudnative-pg/postgresql:18",
		"spec.affinity.topologyKey": "topology.kubernetes.io/zone",
	} {
		value, found := pathutil.Get(wrapped, path)
		if !found {
			t.Errorf("%s was not captured", path)
			continue
		}
		if value != want {
			t.Errorf("%s = %v, want %v", path, value, want)
		}
	}

	if _, found := pathutil.Get(wrapped, "spec.instances"); !found {
		t.Error("spec.instances was not captured")
	}
	if _, found := pathutil.Get(wrapped, "spec.resources.requests.memory"); !found {
		t.Error("spec.resources was not captured")
	}
}

// Opt-in groups must stay out unless named.
func TestOptInGroupsAreNotCapturedByDefault(t *testing.T) {
	cluster := realisticCluster()
	cluster.Spec.PostgresUID = 1001
	cluster.Spec.EnableSuperuserAccess = ptr.To(true)

	got, err := Extract(cluster, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := map[string]any{"spec": got.Spec}
	for _, path := range []string{"spec.postgresUID", "spec.enableSuperuserAccess"} {
		if _, found := pathutil.Get(wrapped, path); found {
			t.Errorf("opt-in path %q captured without being requested", path)
		}
	}

	withOptIn, err := Extract(cluster, ExtractOptions{Groups: []string{"Identity"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := pathutil.Get(map[string]any{"spec": withOptIn.Spec}, "spec.postgresUID"); !found {
		t.Error("Identity group did not capture postgresUID when requested")
	}
}

// Go randomizes map iteration order. If any of that leaked into the encoding,
// the checksum would differ between runs and every reconcile would write a new
// snapshot.
func TestChecksumIsStableAcrossRuns(t *testing.T) {
	first, err := Extract(realisticCluster(), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		next, err := Extract(realisticCluster(), ExtractOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if next.Checksum != first.Checksum {
			t.Fatalf("checksum drifted on run %d: %s != %s", i, next.Checksum, first.Checksum)
		}
	}
}

// The checksum exists to answer "did anything we capture actually change?".
// A generation bump alone must not move it.
func TestChecksumIgnoresGenerationAndTimestamp(t *testing.T) {
	base := realisticCluster()
	first, err := Extract(base, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	bumped := realisticCluster()
	bumped.Generation = 99
	second, err := Extract(bumped, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if first.Checksum != second.Checksum {
		t.Error("checksum changed for a generation bump that touched no captured field")
	}

	changed := realisticCluster()
	changed.Spec.PostgresConfiguration.Parameters["work_mem"] = "8MB"
	third, err := Extract(changed, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Checksum == third.Checksum {
		t.Error("checksum did not change for a real GUC change")
	}
}

// The allowlist means the denylist is normally never exercised: spec.plugins is
// excluded simply by not being in any group. That makes it easy to ship a
// denylist that has quietly stopped working.
//
// This test forces the defence-in-depth path by registering a group that sweeps
// in the entire spec, which is what a careless future edit would look like, and
// asserts the denylist still strips the archiving configuration.
func TestDenylistStripsArchivingEvenWhenAGroupSweepsWholeSpec(t *testing.T) {
	Groups = append(Groups, Group{Name: "Everything", Paths: []string{"spec"}, DefaultEnabled: false})
	defer func() {
		Groups = Groups[:len(Groups)-1]
	}()

	got, err := Extract(realisticCluster(), ExtractOptions{Groups: []string{"Everything"}})
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: the sweep really did capture broadly, otherwise the assertions
	// below would pass for the wrong reason.
	if _, found := pathutil.Get(map[string]any{"spec": got.Spec}, "spec.storage.size"); !found {
		t.Fatal("the whole-spec group captured nothing; test is not exercising the denylist")
	}

	encoded, err := Canonical(got.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"barmanObjectName", "externalClusters", "backups"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("denylist failed to strip %q from a whole-spec capture", forbidden)
		}
	}
}

// A pg_hba rule may address a client set as ${podselector:NAME}, where NAME is
// defined in .spec.podSelectorRefs — which lives on ClusterSpec, not under
// .spec.postgresql. Capturing the rules without the selectors they reference
// would restore host-based authentication pointing at an undefined name.
func TestExtractCapturesPgHBAWithItsPodSelectors(t *testing.T) {
	cluster := realisticCluster()
	cluster.Spec.PostgresConfiguration.PgHBA = []string{
		"host all all ${podselector:app-pods} scram-sha-256",
		"host all all 10.0.0.0/8 md5",
	}
	cluster.Spec.PodSelectorRefs = []apiv1.PodSelectorRef{{
		Name:     "app-pods",
		Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "billing"}},
	}}

	got, err := Extract(cluster, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := map[string]any{"spec": got.Spec}

	rules, found := pathutil.Get(wrapped, "spec.postgresql.pg_hba")
	if !found {
		t.Fatal("pg_hba was not captured")
	}
	if len(rules.([]any)) != 2 {
		t.Errorf("pg_hba = %v, want both rules", rules)
	}

	selectors, found := pathutil.Get(wrapped, "spec.podSelectorRefs")
	if !found {
		t.Fatal("podSelectorRefs was not captured; a restored pg_hba would reference an undefined selector")
	}
	first := selectors.([]any)[0].(map[string]any)
	if first["name"] != "app-pods" {
		t.Errorf("podSelectorRefs[0].name = %v, want app-pods", first["name"])
	}

	// Skipping the group must take both away together, never one without the
	// other.
	skipped, err := Extract(cluster, ExtractOptions{Groups: []string{"Storage"}})
	if err != nil {
		t.Fatal(err)
	}
	onlyStorage := map[string]any{"spec": skipped.Spec}
	if _, found := pathutil.Get(onlyStorage, "spec.postgresql.pg_hba"); found {
		t.Error("pg_hba captured without the Gucs group")
	}
	if _, found := pathutil.Get(onlyStorage, "spec.podSelectorRefs"); found {
		t.Error("podSelectorRefs captured without the Gucs group")
	}
}

func TestExtractCapturesPgIdentAndSharedPreload(t *testing.T) {
	cluster := realisticCluster()
	cluster.Spec.PostgresConfiguration.PgIdent = []string{"mymap /^(.*)@example\\.com$ \\1"}

	got, err := Extract(cluster, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := map[string]any{"spec": got.Spec}
	for _, path := range []string{
		"spec.postgresql.pg_hba",
		"spec.postgresql.pg_ident",
		"spec.postgresql.shared_preload_libraries",
	} {
		if _, found := pathutil.Get(wrapped, path); !found {
			t.Errorf("%s was not captured", path)
		}
	}
}

// The built-in filter covers only what is wrong in every scenario, matched by
// domain so a subdomain is covered and a lookalike is not.
func TestIsDeniedMetadataKey(t *testing.T) {
	for key, want := range map[string]bool{
		"cnpg.io/cluster":                                  true,
		"barmancloud.cnpg.io/x":                            true,
		"chronicle.sharifmshaker.github.io/restored-from":  true,
		"kubectl.kubernetes.io/last-applied-configuration": true,

		"notcnpg.io/x":                      false,
		"app.kubernetes.io/instance":        false,
		"argocd.argoproj.io/tracking-id":    false,
		"kubectl.kubernetes.io/restartedAt": false,
		"example.com/owner":                 false,
		"team":                              false,
	} {
		if got := IsDeniedMetadataKey(key); got != want {
			t.Errorf("IsDeniedMetadataKey(%q) = %v, want %v", key, got, want)
		}
	}
}
