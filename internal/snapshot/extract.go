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
	"fmt"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

// ExtractOptions controls what a capture includes.
type ExtractOptions struct {
	// Groups are the capture groups to include. Empty means DefaultGroupNames().
	Groups []string

	// PluginVersion is recorded in the snapshot's provenance.
	PluginVersion string

	// Now is the capture timestamp. Zero means time.Now().UTC().
	Now time.Time
}

func (o ExtractOptions) groups() []string {
	if len(o.Groups) == 0 {
		return DefaultGroupNames()
	}
	return o.Groups
}

func (o ExtractOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now.UTC()
}

// Extract captures a Cluster's configuration.
//
// The capture is an allowlist walk: only paths named by an enabled Group are
// copied out of the Cluster. Anything CloudNativePG adds in a future release is
// therefore invisible to the plugin until someone adds it to Groups
// deliberately, which is the behaviour we want for something that will later be
// replayed onto a different cluster.
func Extract(cluster *apiv1.Cluster, options ExtractOptions) (*Snapshot, error) {
	if cluster == nil {
		return nil, fmt.Errorf("cannot extract from a nil cluster")
	}

	source, err := pathutil.ToMap(cluster)
	if err != nil {
		return nil, fmt.Errorf("while serializing cluster: %w", err)
	}

	enabled := options.groups()
	captured := map[string]any{}
	metadataEnabled := false

	for _, name := range enabled {
		group, ok := LookupGroup(name)
		if !ok {
			return nil, fmt.Errorf("unknown capture group %q (known: %v)", name, GroupNames())
		}
		if group.Name == MetadataGroupName {
			metadataEnabled = true
		}

		for _, path := range group.Paths {
			if IsDeniedPath(path) {
				// A group definition should never name a denied path. Fail loudly
				// rather than silently dropping it, because it means the two lists
				// have drifted apart.
				return nil, fmt.Errorf("capture group %q names denied path %q", group.Name, path)
			}
			value, found := pathutil.Get(source, path)
			if !found {
				continue
			}
			pathutil.Set(captured, path, value)
		}
	}

	// Defence in depth. The allowlist above should make this a no-op.
	ApplyDenylist(captured)

	sanitizePostgresConfiguration(captured)

	content := Content{}
	if spec, ok := captured["spec"].(map[string]any); ok && len(spec) > 0 {
		content.Spec = spec
	}
	if metadataEnabled {
		content.Labels = FilterMetadata(cluster.Labels)
		content.Annotations = FilterMetadata(cluster.Annotations)
	}

	checksum, err := Checksum(content)
	if err != nil {
		return nil, fmt.Errorf("while checksumming snapshot: %w", err)
	}

	return &Snapshot{
		APIVersion: APIVersion,
		Kind:       Kind,
		CapturedAt: options.now(),
		Generation: cluster.Generation,
		Cluster: ClusterRef{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			UID:       string(cluster.UID),
		},
		Source: SourceInfo{
			PluginVersion: options.PluginVersion,
			ImageName:     cluster.Status.Image,
		},
		Groups:       append([]string(nil), enabled...),
		CaptureRules: CaptureRulesDigest(enabled),
		Content:      content,
		Checksum:     checksum,
	}, nil
}

// sanitizePostgresConfiguration strips the PostgreSQL settings CloudNativePG
// owns from a captured spec.
func sanitizePostgresConfiguration(captured map[string]any) {
	if parameters, found := pathutil.Get(captured, "spec.postgresql.parameters"); found {
		if asMap, ok := parameters.(map[string]any); ok {
			if filtered := FilterGUCs(asMap); filtered != nil {
				pathutil.Set(captured, "spec.postgresql.parameters", filtered)
			} else {
				pathutil.Delete(captured, "spec.postgresql.parameters")
			}
		}
	}

	if libraries, found := pathutil.Get(captured, "spec.postgresql.shared_preload_libraries"); found {
		if asSlice, ok := libraries.([]any); ok {
			if filtered := FilterSharedPreloadLibraries(asSlice); filtered != nil {
				pathutil.Set(captured, "spec.postgresql.shared_preload_libraries", filtered)
			} else {
				pathutil.Delete(captured, "spec.postgresql.shared_preload_libraries")
			}
		}
	}
}
