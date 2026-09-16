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

// Package config reads the plugin's own parameters out of a Cluster.
package config

import (
	"fmt"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
)

// PluginConfiguration is the plugin's view of one Cluster.
//
// CNPG-I passes no separate configuration channel: a plugin's parameters travel
// inside the Cluster JSON at .spec.plugins[] where name matches. That map is
// strictly map[string]string, which is why anything structured (the store, the
// restore policy) is a named reference to one of our CRDs rather than inline
// configuration.
type PluginConfiguration struct {
	// Cluster is the decoded Cluster the hook was called for.
	Cluster *apiv1.Cluster

	// SaveToStore names the ConfigStore snapshots are written to. Empty
	// disables capture.
	SaveToStore string

	// SaveToServer is the server name snapshots are filed under, defaulted to
	// the Cluster name.
	SaveToServer string

	// RestoreFromStore and RestoreFromServer identify the history to restore at
	// creation. Both are required together; see Validate.
	RestoreFromStore  string
	RestoreFromServer string

	// RestorePolicy optionally names the RestorePolicy supplying selection,
	// skip and transform rules. Empty means "restore this history as it was".
	RestorePolicy string

	// Enabled is false when the plugin is listed but explicitly disabled.
	Enabled bool

	// Present is false when the plugin is not listed on this Cluster at all.
	Present bool
}

// Validate reports a configuration that cannot be acted on.
//
// Only the pairing is checked here. Whether the named objects exist, and
// whether their rules make sense, is decided where those objects are read.
func (c *PluginConfiguration) Validate() error {
	if !c.Present || !c.Enabled {
		return nil
	}

	switch {
	case c.RestoreFromStore != "" && c.RestoreFromServer == "":
		return fmt.Errorf(
			"%s is set to %q but %s is missing: a store says where to look, not what to "+
				"look for. Set both, or neither",
			metadata.ParameterRestoreFromStore, c.RestoreFromStore,
			metadata.ParameterRestoreFromServer)

	case c.RestoreFromServer != "" && c.RestoreFromStore == "":
		return fmt.Errorf(
			"%s is set to %q but %s is missing: a server name means nothing without the "+
				"store holding it. Set both, or neither",
			metadata.ParameterRestoreFromServer, c.RestoreFromServer,
			metadata.ParameterRestoreFromStore)

	case c.RestorePolicy != "" && c.RestoreFromStore == "":
		return fmt.Errorf(
			"%s is set to %q but there is nothing to restore: a policy describes how to "+
				"restore, not what. Set %s and %s",
			metadata.ParameterRestorePolicy, c.RestorePolicy,
			metadata.ParameterRestoreFromStore, metadata.ParameterRestoreFromServer)
	}

	return nil
}

// NewFromCluster extracts the plugin configuration from a Cluster.
func NewFromCluster(cluster *apiv1.Cluster) *PluginConfiguration {
	for _, declaration := range cluster.Spec.Plugins {
		if declaration.Name != metadata.PluginName {
			continue
		}

		result := NewFromParameters(declaration.Parameters, cluster.Name)
		result.Cluster = cluster
		result.Present = true
		result.Enabled = declaration.Enabled == nil || *declaration.Enabled
		return result
	}

	return &PluginConfiguration{Cluster: cluster, SaveToServer: cluster.Name}
}

// NewFromParameters builds a configuration from raw plugin parameters.
//
// It exists for the admission webhook, which sees a Cluster as generic JSON
// before the object exists and so cannot decode a typed one. Keeping both
// callers on the same constructor is what stops parameter names and defaulting
// from drifting between capture and restore.
//
// Present and Enabled are the caller's to set: raw parameters cannot say
// whether the plugin was listed at all.
func NewFromParameters(parameters map[string]string, clusterName string) *PluginConfiguration {
	result := &PluginConfiguration{
		// Defaulting the server name to the cluster name matches barman-cloud,
		// so a cluster's snapshots sit beside its own backups without anyone
		// having to configure the correspondence.
		SaveToServer:      clusterName,
		SaveToStore:       parameters[metadata.ParameterSaveToStore],
		RestoreFromStore:  parameters[metadata.ParameterRestoreFromStore],
		RestoreFromServer: parameters[metadata.ParameterRestoreFromServer],
		RestorePolicy:     parameters[metadata.ParameterRestorePolicy],
	}
	if server := parameters[metadata.ParameterSaveToServer]; server != "" {
		result.SaveToServer = server
	}
	return result
}

// IsSaveEnabled reports whether this Cluster should be snapshotted.
func (c *PluginConfiguration) IsSaveEnabled() bool {
	return c.Present && c.Enabled && c.SaveToStore != ""
}

// IsRestoreRequested reports whether this Cluster asked for a restore. The
// policy is optional, so the store is what makes a restore requested.
func (c *PluginConfiguration) IsRestoreRequested() bool {
	return c.Present && c.Enabled && c.RestoreFromStore != ""
}

// SaveToKey is the ConfigStore to write to.
//
// Object references are always same-namespace, matching barman-cloud: a store in
// another namespace would let one tenant read another's configuration history
// through a name they chose themselves. The restore side applies the same rule
// in the admission webhook, which resolves names against the request's
// namespace because the Cluster does not exist yet.
func (c *PluginConfiguration) SaveToKey() types.NamespacedName {
	return types.NamespacedName{Namespace: c.Cluster.Namespace, Name: c.SaveToStore}
}
