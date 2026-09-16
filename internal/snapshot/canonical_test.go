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
	"testing"
)

// The snapshot checksum covers what was captured, not the rules that decided
// it — so enabling a group is invisible to the fast path unless the fingerprint
// says otherwise. Without this, newly-enabled fields sit uncaptured until some
// unrelated edit happens to bump the generation.
func TestMetadataFingerprintChangesWithTheCaptureGroupSet(t *testing.T) {
	labels := map[string]string{"tier": "gold"}

	base, err := MetadataFingerprint(labels, nil, DefaultGroupNames(), "")
	if err != nil {
		t.Fatal(err)
	}
	widened, err := MetadataFingerprint(labels, nil, append(DefaultGroupNames(), "Identity"), "")
	if err != nil {
		t.Fatal(err)
	}
	if base == widened {
		t.Error("enabling a capture group must change the fingerprint, or capture is skipped")
	}
}

// Reordering a list in a spec is not a change, and must not force a re-capture
// of every cluster writing to the store.
func TestMetadataFingerprintIgnoresCaptureGroupOrder(t *testing.T) {
	forward, err := MetadataFingerprint(nil, nil, []string{"Gucs", "Storage", "Resources"}, "")
	if err != nil {
		t.Fatal(err)
	}
	shuffled, err := MetadataFingerprint(nil, nil, []string{"Resources", "Gucs", "Storage"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if forward != shuffled {
		t.Error("capture-group order must not affect the fingerprint")
	}
}

// The groups still have to be distinguishable from labels: a group named X and
// a label named X must not collide into the same fingerprint.
func TestMetadataFingerprintSeparatesGroupsFromLabels(t *testing.T) {
	asGroup, err := MetadataFingerprint(nil, nil, []string{"Gucs"}, "")
	if err != nil {
		t.Fatal(err)
	}
	asLabel, err := MetadataFingerprint(map[string]string{"Gucs": ""}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if asGroup == asLabel {
		t.Error("a capture group and a label of the same name must not produce the same fingerprint")
	}
}

// A snapshot written without the Metadata group carries no labels, and the
// watermark recomputes the fingerprint from that snapshot. Hashing the live
// Cluster's labels anyway would make the two disagree forever, and the fast
// path would never hit for a store that leaves the group out.
func TestMetadataFingerprintIgnoresLabelsWithoutTheMetadataGroup(t *testing.T) {
	groups := []string{"Gucs", "Storage"}

	withLabels, err := MetadataFingerprint(map[string]string{"tier": "gold"}, nil, groups, "")
	if err != nil {
		t.Fatal(err)
	}
	withoutLabels, err := MetadataFingerprint(nil, nil, groups, "")
	if err != nil {
		t.Fatal(err)
	}
	if withLabels != withoutLabels {
		t.Error("labels must not affect the fingerprint when the Metadata group is not captured")
	}

	captured, err := MetadataFingerprint(map[string]string{"tier": "gold"}, nil, append(groups, MetadataGroupName), "")
	if err != nil {
		t.Fatal(err)
	}
	if captured == withoutLabels {
		t.Error("labels must affect the fingerprint once the Metadata group is captured")
	}
}

// The rules digest is what lets a plugin upgrade that captures more be noticed
// without an unrelated edit. It must move when a group gains a path, and must
// not move when the same groups are merely listed in another order.
func TestCaptureRulesDigest(t *testing.T) {
	base := CaptureRulesDigest([]string{"Gucs", "Timings"})
	if again := CaptureRulesDigest([]string{"Timings", "Gucs"}); again != base {
		t.Error("the digest depends on the order groups are listed in")
	}
	if other := CaptureRulesDigest([]string{"Gucs"}); other == base {
		t.Error("a different group selection produced the same digest")
	}

	timings := -1
	for i, group := range Groups {
		if group.Name == "Timings" {
			timings = i
		}
	}
	saved := Groups[timings].Paths
	Groups[timings].Paths = append(append([]string(nil), saved...), "spec.somethingNew")
	defer func() {
		Groups[timings].Paths = saved
	}()

	if grown := CaptureRulesDigest([]string{"Gucs", "Timings"}); grown == base {
		t.Error("adding a path to a group left the digest unchanged, so the upgrade would never be captured")
	}
}

func TestMetadataFingerprintChangesWithTheCaptureRules(t *testing.T) {
	groups := DefaultGroupNames()
	before, err := MetadataFingerprint(nil, nil, groups, "sha256:old")
	if err != nil {
		t.Fatal(err)
	}
	after, err := MetadataFingerprint(nil, nil, groups, "sha256:new")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("the fingerprint ignored a change to the capture rules")
	}
}
