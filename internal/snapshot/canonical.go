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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
)

// Canonical encodes content deterministically.
//
// Determinism comes from two places. encoding/json sorts map keys, so the
// traversal order of Go maps cannot leak into the output; and numbers arrive as
// json.Number (see pathutil.ToMap), so they re-encode with the exact digits
// they had in the source rather than being routed through float64. Slice order
// is preserved, which is correct: the order of pg_hba rules or tolerations is
// semantic.
func Canonical(content Content) ([]byte, error) {
	return json.Marshal(content)
}

// Checksum returns a stable sha256 over the canonical encoding of content,
// prefixed with its algorithm.
//
// This is what lets a generation bump that touched nothing we capture avoid a
// pointless write to the object store.
func Checksum(content Content) (string, error) {
	encoded, err := Canonical(content)
	if err != nil {
		return "", err
	}
	return digest(encoded), nil
}

// MetadataFingerprint is the cheap key the capture fast path compares: the
// labels and annotations a capture would keep, the capture groups, and the
// digest of the rules those groups were captured under.
//
// Labels and annotations are here because Kubernetes does not bump
// metadata.generation for edits to them. A capture trigger based on generation
// alone would never notice a label being added, and the change would sit
// uncaptured until some unrelated spec edit happened to flush it out. They count
// only when the Metadata group is captured, because only then does a snapshot
// carry them: the fingerprint is compared against one recomputed from the newest
// snapshot, and hashing labels a snapshot never holds would make the two
// disagree forever.
//
// The capture rules are here for the opposite reason: they change what a
// capture produces while generation and labels stay put. Enabling a group on a
// store, or a plugin upgrade that adds a path to a group or changes a filter,
// would otherwise be invisible to the fast path, and the newly captured fields
// would sit unwritten until some unrelated edit.
//
// This is deliberately much cheaper than a full Extract: it touches two small
// maps and two short strings, so the hot reconcile path can evaluate it on
// every pass without going near the object store.
func MetadataFingerprint(
	labels, annotations map[string]string,
	groups []string,
	captureRules string,
) (string, error) {
	if !slices.Contains(groups, MetadataGroupName) {
		labels, annotations = nil, nil
	}

	fingerprint := struct {
		Content
		CaptureGroups []string `json:"captureGroups,omitempty"`
		CaptureRules  string   `json:"captureRules,omitempty"`
	}{
		Content: Content{
			Labels:      FilterMetadata(labels),
			Annotations: FilterMetadata(annotations),
		},
		// Sorted so that reordering the list in a spec is not a change.
		CaptureGroups: slices.Sorted(slices.Values(groups)),
		CaptureRules:  captureRules,
	}

	// Marshalled here rather than through Canonical, which is typed to Content
	// because it defines a snapshot's on-disk form. This is a local fingerprint,
	// never written anywhere, so it does not belong to that contract.
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return "", err
	}
	return digest(encoded), nil
}

// CaptureRulesDigest hashes every rule that decides what a capture with these
// groups produces: each group's paths, the path denylist, the metadata key
// filter, and CloudNativePG's lists of fixed parameters and operator-managed
// libraries.
//
// It is data, derived rather than versioned by hand, so a change to any of those
// lists — including one arriving through a CloudNativePG dependency bump —
// changes the digest without anyone remembering to. What it cannot see is a
// change to Extract's own logic, which is rare and shows up in review.
func CaptureRulesDigest(groups []string) string {
	selected := slices.Sorted(slices.Values(groups))
	paths := make(map[string][]string, len(selected))
	for _, name := range selected {
		group, _ := LookupGroup(name)
		paths[name] = group.Paths
	}

	rules := struct {
		Groups                map[string][]string `json:"groups"`
		DeniedPaths           []string            `json:"deniedPaths"`
		DeniedMetadataDomains []string            `json:"deniedMetadataDomains"`
		DeniedMetadataKeys    []string            `json:"deniedMetadataKeys"`
		FixedParameters       []string            `json:"fixedParameters"`
		ManagedLibraries      []string            `json:"managedLibraries"`
	}{
		Groups:                paths,
		DeniedPaths:           DeniedPaths,
		DeniedMetadataDomains: deniedMetadataDomains,
		DeniedMetadataKeys:    deniedMetadataKeys,
		FixedParameters:       slices.Sorted(maps.Keys(postgres.FixedConfigurationParameters)),
		ManagedLibraries:      slices.Sorted(maps.Keys(managedLibraries)),
	}

	// Every field is a string, a slice of strings or a map of them, which
	// cannot fail to marshal.
	encoded, _ := json.Marshal(rules)
	return digest(encoded)
}

func digest(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}
