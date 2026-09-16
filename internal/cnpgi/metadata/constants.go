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

// Package metadata holds the plugin's identity constants.
package metadata

import "github.com/cloudnative-pg/cnpg-i/pkg/identity"

const (
	// PluginName is the CNPG-I plugin name. It must match both the
	// cnpg.io/pluginName label on the plugin Service and the name used in a
	// Cluster's .spec.plugins[].
	PluginName = "chronicle.sharifmshaker.github.io"

	// Version is the plugin version reported to the operator.
	Version = "0.1.0"
)

// Plugin parameter keys accepted in .spec.plugins[].parameters.
//
// The save and restore sides are deliberately symmetric. Each names a store and
// a server name, so "where does this write" and "where does this read" are
// answered the same way and read the same way in a manifest.
const (
	// ParameterSaveToStore names the ConfigStore snapshots are written to.
	// Without it the cluster is not captured at all, which is a valid way to
	// run: a cluster may restore from a history it does not contribute to.
	ParameterSaveToStore = "saveToStore"

	// ParameterSaveToServer overrides the server name snapshots are filed
	// under. Defaults to the Cluster name, matching barman-cloud's convention.
	ParameterSaveToServer = "saveToServer"

	// ParameterRestoreFromStore names the ConfigStore to restore from.
	ParameterRestoreFromStore = "restoreFromStore"

	// ParameterRestoreFromServer names the server within that store whose
	// history to restore.
	//
	// Both restoreFrom parameters are required together: a store without a
	// server says where to look but not for what, and a server without a store
	// the reverse. Either alone is refused at admission rather than guessed at.
	ParameterRestoreFromServer = "restoreFromServer"

	// ParameterRestorePolicy optionally names a RestorePolicy: the rules for
	// which snapshot to select, what to leave alone and what to rewrite.
	//
	// Optional because the common case needs no rules — "restore this history
	// as it was" is fully specified by the two restoreFrom parameters. A policy
	// carries no source of its own, so one policy applies to any number of
	// restores from any number of stores.
	ParameterRestorePolicy = "restorePolicy"
)

// MutatingWebhookName is the MutatingWebhookConfiguration that serves restore.
// It appears in the Pre-reconcile guard's error so an operator whose restore
// never ran knows which object to look for.
const MutatingWebhookName = "chronicle-restore.sharifmshaker.github.io"

// Annotations the plugin sets on a Cluster.
const (
	// AnnotationRestoredFrom records the snapshot a Cluster was restored from.
	// The Pre-reconcile guard treats its absence, when restoreFrom is configured,
	// as proof the mutating webhook never ran.
	//
	// Writing an annotation does not bump metadata.generation (the API server
	// only increments it when something outside metadata changes), so this
	// cannot re-trigger the snapshot path.
	AnnotationRestoredFrom = "chronicle.sharifmshaker.github.io/restored-from"
)

// Data is the plugin metadata returned over the Identity service.
var Data = identity.GetPluginMetadataResponse{
	Name:          PluginName,
	Version:       Version,
	DisplayName:   "CloudNativePG Chronicle",
	Description:   "Snapshots CloudNativePG Cluster configuration to an object store and restores it on cluster creation.",
	ProjectUrl:    "https://github.com/sharifmshaker/cnpg-i-chronicle",
	RepositoryUrl: "https://github.com/sharifmshaker/cnpg-i-chronicle",
	License:       "Apache-2.0",
	LicenseUrl:    "https://github.com/sharifmshaker/cnpg-i-chronicle/blob/main/LICENSE",
	Maturity:      "alpha",
}
