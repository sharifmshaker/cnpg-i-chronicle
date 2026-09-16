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

package store

import (
	"fmt"
	"testing"
	"time"
)

var retentionNow = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// refsAt builds snapshot refs the given number of days before `retentionNow`,
// oldest first, with keys in the real content-addressed format.
func refsAt(daysAgo ...int) []SnapshotRef {
	refs := make([]SnapshotRef, 0, len(daysAgo))
	for i, d := range daysAgo {
		at := retentionNow.AddDate(0, 0, -d)
		refs = append(refs, SnapshotRef{
			Key:        fmt.Sprintf("pg/chronicle/snapshots/%s-g%010d-abcd1234.json", at.Format("20060102T150405Z"), i+1),
			CapturedAt: at,
			Generation: int64(i + 1),
		})
	}
	return refs
}

func keysOf(refs []SnapshotRef, indexes ...int) []string {
	out := make([]string, 0, len(indexes))
	for _, i := range indexes {
		out = append(out, refs[i].Key)
	}
	return out
}

func sameKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d keys %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The case that makes this different from backup retention. A cluster whose
// configuration has not changed in a year has exactly one snapshot, a year old,
// and it describes what is running right now. Deleting it for being older than
// the window would leave every restore in that window with nothing to select.
func TestPlanRetentionKeepsTheOnlySnapshotHoweverOld(t *testing.T) {
	refs := refsAt(365)
	if got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30)); got != nil {
		t.Errorf("deleted %v; a lone snapshot is the anchor for every target", got)
	}
}

// Same principle with history behind it: the newest snapshot before the window
// is the anchor and survives, however old. Only what it supersedes goes.
func TestPlanRetentionKeepsTheAnchorAndDropsWhatItSupersedes(t *testing.T) {
	// 400 and 350 days ago are superseded by the 300-day-old anchor; 10 and 2
	// are inside the window.
	refs := refsAt(400, 350, 300, 10, 2)
	got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30))
	sameKeys(t, got, keysOf(refs, 0, 1))
}

// Nothing outside the window means nothing is superseded.
func TestPlanRetentionDeletesNothingWhenAllAreInsideTheWindow(t *testing.T) {
	refs := refsAt(20, 10, 1)
	if got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30)); got != nil {
		t.Errorf("deleted %v; every snapshot is inside the window", got)
	}
}

// Every snapshot older than the window still leaves the newest of them as the
// anchor — the configuration in force across the whole window.
func TestPlanRetentionKeepsTheNewestWhenAllAreOutsideTheWindow(t *testing.T) {
	refs := refsAt(400, 300, 200)
	got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30))
	sameKeys(t, got, keysOf(refs, 0, 1))
}

// A snapshot exactly at the cutoff is inside the window: the guarantee is
// "at or after".
func TestPlanRetentionTreatsTheCutoffAsInsideTheWindow(t *testing.T) {
	refs := refsAt(60, 30)
	got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30))
	if len(got) != 0 {
		t.Errorf("deleted %v; the 30-day-old snapshot sits exactly at the cutoff and anchors it", got)
	}
}

func TestPlanRetentionHandlesEmptyAndSingle(t *testing.T) {
	if got := PlanRetention(nil, retentionNow); got != nil {
		t.Errorf("got %v for no snapshots", got)
	}
	if got := PlanRetention(refsAt(999), retentionNow); got != nil {
		t.Errorf("got %v for a single snapshot", got)
	}
}

// Order in equals order out: the caller may pass refs in any order, and two
// captures can share a second, so the key breaks the tie.
func TestPlanRetentionIsIndependentOfInputOrder(t *testing.T) {
	refs := refsAt(400, 350, 300, 10)
	cutoff := retentionNow.AddDate(0, 0, -30)

	forward := PlanRetention(refs, cutoff)
	reversed := PlanRetention([]SnapshotRef{refs[3], refs[2], refs[1], refs[0]}, cutoff)
	sameKeys(t, reversed, forward)
}

// The smallest deletion there is: one superseded snapshot, with the anchor
// immediately after it. Worth pinning on its own — an off-by-one on the anchor
// index leaves this case silently doing nothing while every larger case works.
func TestPlanRetentionDeletesASingleSupersededSnapshot(t *testing.T) {
	refs := refsAt(400, 300, 10)
	got := PlanRetention(refs, retentionNow.AddDate(0, 0, -30))
	sameKeys(t, got, keysOf(refs, 0))
}
