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

package config

import (
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
)

func cluster(plugins ...apiv1.PluginConfiguration) *apiv1.Cluster {
	return &apiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-source", Namespace: "team-a"},
		Spec:       apiv1.ClusterSpec{Plugins: plugins},
	}
}

func ours(parameters map[string]string) apiv1.PluginConfiguration {
	return apiv1.PluginConfiguration{Name: metadata.PluginName, Parameters: parameters}
}

// Opting in is exactly "the plugin is listed, enabled, and names a store".
// Every capture decision and the orphan sweep rest on this, so each way of
// being opted out is worth pinning separately.
func TestIsSaveEnabled(t *testing.T) {
	disabled := ours(map[string]string{metadata.ParameterSaveToStore: "config-store"})
	disabled.Enabled = ptr.To(false)

	explicitlyEnabled := ours(map[string]string{metadata.ParameterSaveToStore: "config-store"})
	explicitlyEnabled.Enabled = ptr.To(true)

	for _, tc := range []struct {
		name    string
		cluster *apiv1.Cluster
		want    bool
	}{
		{"listed with a store", cluster(ours(map[string]string{metadata.ParameterSaveToStore: "config-store"})), true},
		{"explicitly enabled", cluster(explicitlyEnabled), true},
		{"not listed at all", cluster(), false},
		{"listed but disabled", cluster(disabled), false},
		{"listed with no saveTo", cluster(ours(map[string]string{})), false},
		{"only restoreFrom", cluster(ours(map[string]string{metadata.ParameterRestoreFromStore: "from-prod"})), false},
		{"someone else's plugin", cluster(apiv1.PluginConfiguration{
			Name:       "barman-cloud.cloudnative-pg.io",
			Parameters: map[string]string{metadata.ParameterSaveToStore: "config-store"},
		}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewFromCluster(tc.cluster).IsSaveEnabled(); got != tc.want {
				t.Errorf("IsSaveEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRestoreRequested(t *testing.T) {
	disabled := ours(map[string]string{metadata.ParameterRestoreFromStore: "from-prod"})
	disabled.Enabled = ptr.To(false)

	for _, tc := range []struct {
		name    string
		cluster *apiv1.Cluster
		want    bool
	}{
		{"listed with a policy", cluster(ours(map[string]string{metadata.ParameterRestoreFromStore: "from-prod"})), true},
		{"not listed", cluster(), false},
		{"disabled", cluster(disabled), false},
		{"only saveTo", cluster(ours(map[string]string{metadata.ParameterSaveToStore: "config-store"})), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewFromCluster(tc.cluster).IsRestoreRequested(); got != tc.want {
				t.Errorf("IsRestoreRequested() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The server name defaults to the cluster name so snapshots land beside the
// cluster's own barman-cloud backups without anyone configuring it.
func TestServerNameDefaultsToTheClusterName(t *testing.T) {
	if got := NewFromCluster(cluster(ours(nil))).SaveToServer; got != "pg-source" {
		t.Errorf("ServerName = %q, want the cluster name pg-source", got)
	}
	if got := NewFromCluster(cluster()).SaveToServer; got != "pg-source" {
		t.Errorf("a cluster without the plugin should still default: got %q", got)
	}
	explicit := ours(map[string]string{metadata.ParameterSaveToServer: "archived-eu"})
	if got := NewFromCluster(cluster(explicit)).SaveToServer; got != "archived-eu" {
		t.Errorf("ServerName = %q, want the override archived-eu", got)
	}
	// An empty override must not erase the default into "".
	empty := ours(map[string]string{metadata.ParameterSaveToServer: ""})
	if got := NewFromCluster(cluster(empty)).SaveToServer; got != "pg-source" {
		t.Errorf("an empty serverName should fall back to the cluster name; got %q", got)
	}
}

// References are same-namespace by construction: a store named from another
// namespace would let one tenant read another's configuration history.
func TestKeysAreAlwaysTheClustersOwnNamespace(t *testing.T) {
	configuration := NewFromCluster(cluster(ours(map[string]string{
		metadata.ParameterSaveToStore: "config-store",
	})))

	key := configuration.SaveToKey()
	if key.Namespace != "team-a" {
		t.Errorf("SaveToKey namespace = %q, want the cluster's own team-a", key.Namespace)
	}
	if key.Name != "config-store" {
		t.Errorf("SaveToKey name = %q", key.Name)
	}
}

// Only the first matching declaration is read; a duplicate entry must not
// silently override the first.
func TestOnlyTheFirstDeclarationIsRead(t *testing.T) {
	configuration := NewFromCluster(cluster(
		ours(map[string]string{metadata.ParameterSaveToStore: "meta-first"}),
		ours(map[string]string{metadata.ParameterSaveToStore: "meta-second"}),
	))
	if configuration.SaveToStore != "meta-first" {
		t.Errorf("SaveToStore = %q, want meta-first", configuration.SaveToStore)
	}
}
