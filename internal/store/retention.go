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
	"sort"
	"time"
)

// PlanRetention returns the snapshot keys that may be deleted while still
// guaranteeing that any restore target at or after cutoff resolves.
//
// The rule is not "delete what is older than cutoff". Snapshots are written
// only when configuration changes, so the newest snapshot at or before the
// cutoff describes the configuration in force across the whole span from then
// until the next change — possibly right up to now. That snapshot is the anchor,
// and it is the answer to every target between the cutoff and whatever came
// next. Deleting it because of its age would leave those restores selecting
// nothing, which is how a cluster untouched for a year loses the only record of
// how it is configured.
//
// So: keep everything at or after the cutoff, keep the anchor however old it is,
// and delete only what the anchor supersedes — history no target inside the
// window could ever select.
//
// Returns nil when there is nothing to remove, which is the common case.
//
// This function deletes data, and the two ways it can be subtly wrong are both
// silent — nothing fails, you simply lose the ability to restore later. Line
// coverage does not help: both mutations below execute every line. If you
// change the anchor arithmetic, re-run them and check the named tests fail.
//
//	anchor < 1  ->  anchor < 0, and range ordered[:anchor] -> ordered[:anchor+1]
//	    The naive "delete everything older than the cutoff" rule, which takes
//	    the anchor with it. Must fail four tests, among them
//	    KeepsTheAnchorAndDropsWhatItSupersedes and
//	    KeepsTheNewestWhenAllAreOutsideTheWindow.
//
//	anchor < 1  ->  anchor < 2
//	    An off-by-one that quietly does nothing when exactly one snapshot is
//	    superseded, while every larger case still works. Must fail
//	    DeletesASingleSupersededSnapshot, and only that one — which is the
//	    reason that test exists: this mutation once passed the whole suite.
//
// Neither mutation reaches KeepsTheOnlySnapshotHoweverOld; the len(refs) < 2
// early return guards it, so that case is pinned by the guard rather than by
// the anchor arithmetic.
func PlanRetention(refs []SnapshotRef, cutoff time.Time) []string {
	if len(refs) < 2 {
		// One snapshot is always the anchor for something. Never delete it.
		return nil
	}

	// Keys are content-addressed and timestamp-prefixed, so key order is
	// chronological and settles ties between two captures in the same second.
	ordered := make([]SnapshotRef, len(refs))
	copy(ordered, refs)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })

	anchor := -1
	for i, ref := range ordered {
		if ref.CapturedAt.Before(cutoff) {
			anchor = i
			continue
		}
		break
	}

	// Everything is already inside the window, or the oldest snapshot is itself
	// the anchor: nothing is superseded.
	if anchor < 1 {
		return nil
	}

	superseded := make([]string, 0, anchor)
	for _, ref := range ordered[:anchor] {
		superseded = append(superseded, ref.Key)
	}
	return superseded
}
