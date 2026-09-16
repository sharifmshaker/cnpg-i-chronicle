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

// Two captures in the same second at the same generation — exactly the case
// content-addressed keys exist for, since a label edit does not bump
// generation. On (CapturedAt, Generation) alone they compare equal, and
// sort.Slice is not stable, so without the key tiebreak their order would vary
// between calls. Selection walks this list to answer "newest at or before T",
// so two runs could resolve the same restore to different snapshots.
func TestListOrderIsTotal(t *testing.T) {
	at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	a := SnapshotRef{Key: "pg/chronicle/snapshots/20260827T120000Z-g0000000001-aaaaaaaa.json", CapturedAt: at, Generation: 1}
	b := SnapshotRef{Key: "pg/chronicle/snapshots/20260827T120000Z-g0000000001-bbbbbbbb.json", CapturedAt: at, Generation: 1}

	refs := []SnapshotRef{a, b}
	sortRefs(refs)
	want := fmt.Sprint(refs)

	for i := range 200 {
		shuffled := []SnapshotRef{a, b}
		if i%2 == 1 {
			shuffled = []SnapshotRef{b, a}
		}
		sortRefs(shuffled)
		if got := fmt.Sprint(shuffled); got != want {
			t.Fatalf("iteration %d ordered differently:\n  got  %s\n  want %s", i, got, want)
		}
	}

	if refs[0].Key >= refs[1].Key {
		t.Error("the key tiebreak should order aaaaaaaa before bbbbbbbb")
	}
}
