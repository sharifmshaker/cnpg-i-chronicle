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
	"slices"
	"strings"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

// DeniedPaths are never captured and never restored, and no configuration can
// re-enable them.
//
// The first three are the reason this list is not merely advisory. A Cluster's
// .spec.plugins, .spec.backup and .spec.externalClusters describe *where this
// cluster archives its WAL*. Copying them from a source cluster into a restored
// one would point two live clusters at the same server name in the same object
// store, interleaving their WAL streams and corrupting the backup history of
// the cluster that was working fine. There is no use case that justifies it, so
// it is not a default — it is a hard rule.
var DeniedPaths = []string{
	"spec.plugins",
	"spec.backup",
	"spec.externalClusters",

	// Bootstrap is honoured only at creation and is the user's own restore
	// intent. Overwriting it with the source cluster's bootstrap would replace
	// "recover from this backup" with "initdb a fresh database".
	"spec.bootstrap",

	// Replica-cluster topology is a property of the deployment, not of the
	// configuration being carried across.
	"spec.replica",

	// Identity and server-side state.
	"metadata.name",
	"metadata.namespace",
	"metadata.uid",
	"metadata.resourceVersion",
	"metadata.generation",
	"metadata.creationTimestamp",
	"metadata.deletionTimestamp",
	"metadata.ownerReferences",
	"metadata.managedFields",
	"metadata.selfLink",
	"metadata.finalizers",
	"status",
}

// deniedMetadataDomains own label and annotation keys that are wrong to carry
// to another cluster in every scenario, matched with their subdomains:
// CloudNativePG's own bookkeeping, and this plugin's restore record.
//
// The list is deliberately short. Whether a key set by some other tool should
// travel — Argo CD's tracking label, Helm's release annotations — depends on how
// the target cluster is managed, and that is what a RestorePolicy skip rule
// with keyPrefix is for. Guessing here would take the choice away from the
// person who knows.
var deniedMetadataDomains = []string{
	"cnpg.io",
	metadata.PluginName,
}

// deniedMetadataKeys are individual keys to drop.
var deniedMetadataKeys = []string{
	// The source cluster's own manifest, which is neither configuration nor
	// small.
	"kubectl.kubernetes.io/last-applied-configuration",
}

// ApplyDenylist removes every denied path from a generic Cluster map, in place.
// It is called after the capture allowlist has already run, as defence in depth:
// the allowlist should never have admitted these, and if a future edit to
// Groups lets one through, this still catches it.
func ApplyDenylist(object map[string]any) {
	for _, path := range DeniedPaths {
		pathutil.Delete(object, path)
	}
}

// IsDeniedPath reports whether a path is covered by the hard denylist, either
// exactly or as a descendant.
func IsDeniedPath(path string) bool {
	for _, denied := range DeniedPaths {
		if path == denied || strings.HasPrefix(path, denied+".") {
			return true
		}
	}
	return false
}

// FilterMetadata drops the keys IsDeniedMetadataKey refuses from a label or
// annotation map, returning nil when nothing survives.
func FilterMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for key, value := range in {
		if IsDeniedMetadataKey(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsDeniedMetadataKey reports whether a label or annotation key is never
// captured and never restored.
func IsDeniedMetadataKey(key string) bool {
	if slices.Contains(deniedMetadataKeys, key) {
		return true
	}
	domain, _, prefixed := strings.Cut(key, "/")
	if !prefixed {
		return false
	}
	for _, denied := range deniedMetadataDomains {
		if domain == denied || strings.HasSuffix(domain, "."+denied) {
			return true
		}
	}
	return false
}

// FilterGUCs removes PostgreSQL parameters that CloudNativePG manages itself.
//
// The list is imported from the operator rather than copied here on purpose.
// CNPG's admission webhook rejects any parameter in FixedConfigurationParameters
// whose value differs from the one it would generate, and that generated value
// shifts with operator version, replica-cluster state and the
// IsWalArchivingDisabled annotation. Round-tripping such a parameter would
// therefore work until it silently did not. Dropping them is the only stable
// behaviour, and importing the map means a new fixed parameter upstream is
// handled by a dependency bump rather than by remembering to edit a list.
func FilterGUCs(parameters map[string]any) map[string]any {
	if len(parameters) == 0 {
		return nil
	}

	out := make(map[string]any, len(parameters))
	for key, value := range parameters {
		if _, fixed := postgres.FixedConfigurationParameters[key]; fixed {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// managedLibraries are the shared_preload_libraries entries CloudNativePG
// injects itself, flattened once from the operator's own list.
var managedLibraries = func() map[string]struct{} {
	out := map[string]struct{}{}
	for _, extension := range postgres.ManagedExtensions {
		for _, library := range extension.SharedPreloadLibraries {
			out[library] = struct{}{}
		}
	}
	return out
}()

// FilterSharedPreloadLibraries removes the extensions CloudNativePG injects into
// shared_preload_libraries on its own.
//
// CNPG adds and removes these based on whether the user has set any GUC in the
// extension's namespace (see postgres.IsManagedExtensionUsed). Restoring them
// explicitly would either duplicate what the operator is about to add, or pin a
// library whose triggering GUC was skipped by a restore policy.
func FilterSharedPreloadLibraries(libraries []any) []any {
	if len(libraries) == 0 {
		return nil
	}

	out := make([]any, 0, len(libraries))
	for _, library := range libraries {
		if name, ok := library.(string); ok {
			if _, isManaged := managedLibraries[name]; isManaged {
				continue
			}
		}
		out = append(out, library)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
