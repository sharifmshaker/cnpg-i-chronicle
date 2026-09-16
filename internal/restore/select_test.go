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

package restore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

type fakeSource struct {
	refs  []store.SnapshotRef
	byKey map[string]*snapshot.Snapshot
}

func (f *fakeSource) List(context.Context) ([]store.SnapshotRef, error) { return f.refs, nil }

func (f *fakeSource) Read(_ context.Context, key string) (*snapshot.Snapshot, error) {
	snap, ok := f.byKey[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return snap, nil
}

func (f *fakeSource) Latest(ctx context.Context) (*snapshot.Snapshot, string, error) {
	if len(f.refs) == 0 {
		return nil, "", store.ErrNotFound
	}
	key := f.refs[len(f.refs)-1].Key
	snap, err := f.Read(ctx, key)
	return snap, key, err
}

var base = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// history builds three captures an hour apart at generations 1, 2 and 2 — the
// repeated generation models a label edit, which does not bump generation.
func history() *fakeSource {
	source := &fakeSource{byKey: map[string]*snapshot.Snapshot{}}
	entries := []struct {
		offset     time.Duration
		generation int64
	}{
		{0, 1}, {1 * time.Hour, 2}, {2 * time.Hour, 2},
	}
	layout := store.Layout{ServerName: "pg-source", Prefix: "chronicle"}
	for i, entry := range entries {
		at := base.Add(entry.offset)
		key := layout.SnapshotKey(at, entry.generation, "sha256:0000000"+string(rune('a'+i)))
		source.refs = append(source.refs, store.SnapshotRef{Key: key, CapturedAt: at, Generation: entry.generation})
		source.byKey[key] = &snapshot.Snapshot{
			APIVersion: snapshot.APIVersion, Kind: snapshot.Kind,
			CapturedAt: at, Generation: entry.generation,
		}
	}
	return source
}

func policy(mode chroniclev1.SelectMode, generation *int64, at *time.Time) *chroniclev1.RestorePolicy {
	p := &chroniclev1.RestorePolicy{
		Spec: chroniclev1.RestorePolicySpec{
			Select: chroniclev1.RestoreSelect{Mode: mode, Generation: generation},
		},
	}
	if at != nil {
		p.Spec.Select.Timestamp = &metav1.Time{Time: *at}
	}
	return p
}

func TestSelectLatest(t *testing.T) {
	got, err := Select(context.Background(), history(), chroniclev1.SelectLatest, policy(chroniclev1.SelectLatest, nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Snapshot.CapturedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("CapturedAt = %v, want the newest", got.Snapshot.CapturedAt)
	}
}

// An empty mode must behave as Latest, so an object built outside the API
// server (where the CRD default applies) still works.
func TestSelectDefaultsToLatest(t *testing.T) {
	empty := &chroniclev1.RestorePolicy{}
	got, err := Select(context.Background(), history(),
		chroniclev1.ResolveMode(empty, false), empty, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.Generation != 2 {
		t.Errorf("Generation = %d, want 2", got.Snapshot.Generation)
	}
}

// A generation can appear more than once. The newest capture at that
// generation is the most complete, so it wins.
func TestSelectByGenerationPrefersNewestCapture(t *testing.T) {
	two := int64(2)
	got, err := Select(context.Background(), history(), chroniclev1.SelectGeneration, policy(chroniclev1.SelectGeneration, &two, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Snapshot.CapturedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("CapturedAt = %v, want the later of the two generation-2 captures", got.Snapshot.CapturedAt)
	}
}

func TestSelectByGenerationMissing(t *testing.T) {
	missing := int64(99)
	_, err := Select(context.Background(), history(), chroniclev1.SelectGeneration, policy(chroniclev1.SelectGeneration, &missing, nil), nil)
	if err == nil {
		t.Fatal("expected an error for a generation that was never captured")
	}
	// The message should tell the operator what is actually available.
	if !strings.Contains(err.Error(), "generations 1 to 2") {
		t.Errorf("error does not describe the available range: %v", err)
	}
}

func TestSelectByTimestamp(t *testing.T) {
	cases := []struct {
		name   string
		at     time.Time
		want   time.Time
		errors bool
	}{
		{name: "between captures picks the earlier", at: base.Add(90 * time.Minute), want: base.Add(time.Hour)},
		{name: "exactly at a capture includes it", at: base.Add(time.Hour), want: base.Add(time.Hour)},
		{name: "after everything picks the newest", at: base.Add(72 * time.Hour), want: base.Add(2 * time.Hour)},
		{name: "exactly at the first capture", at: base, want: base},
		{name: "before all history fails", at: base.Add(-time.Hour), errors: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := tc.at
			got, err := Select(context.Background(), history(), chroniclev1.SelectTimestamp, policy(chroniclev1.SelectTimestamp, nil, &at), nil)
			if tc.errors {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "does not reach back") {
					t.Errorf("unhelpful error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.Snapshot.CapturedAt.Equal(tc.want) {
				t.Errorf("CapturedAt = %v, want %v", got.Snapshot.CapturedAt, tc.want)
			}
		})
	}
}

func TestSelectOnEmptyHistory(t *testing.T) {
	empty := &fakeSource{byKey: map[string]*snapshot.Snapshot{}}
	at := base
	if _, err := Select(context.Background(), empty, chroniclev1.SelectTimestamp,
		policy(chroniclev1.SelectTimestamp, nil, &at), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSelectRejectsIncompletePolicy(t *testing.T) {
	if _, err := Select(context.Background(), history(), chroniclev1.SelectGeneration, policy(chroniclev1.SelectGeneration, nil, nil), nil); err == nil {
		t.Error("Generation mode without a generation should fail")
	}
	if _, err := Select(context.Background(), history(), chroniclev1.SelectTimestamp, policy(chroniclev1.SelectTimestamp, nil, nil), nil); err == nil {
		t.Error("Timestamp mode without a timestamp should fail")
	}
}

// Retention and selection compose: what survives a sweep must still answer
// every target inside the window, and must refuse the ones outside it rather
// than pairing recovered data with a configuration never in force alongside it.
//
// This is the guarantee spec.retention actually makes, exercised end to end
// against the same PlanRetention the sweep uses.
func TestRetainedHistoryAnswersTheWindowAndRefusesOlderTargets(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	source := &fakeSource{byKey: map[string]*snapshot.Snapshot{}}
	for _, daysAgo := range []int{400, 300, 5} {
		at := now.AddDate(0, 0, -daysAgo)
		key := store.Layout{ServerName: "pg", Prefix: "chronicle"}.SnapshotKey(at, 1, "abcd1234")
		source.refs = append(source.refs, store.SnapshotRef{Key: key, CapturedAt: at, Generation: 1})
		source.byKey[key] = &snapshot.Snapshot{
			CapturedAt: at,
			Generation: 1,
			Content:    snapshot.Content{Spec: map[string]any{"instances": float64(daysAgo)}},
		}
	}

	// Apply a 30-day retention exactly as the sweep does.
	superseded := store.PlanRetention(source.refs, now.AddDate(0, 0, -30))
	if len(superseded) != 1 {
		t.Fatalf("expected the 400-day snapshot to be superseded, got %v", superseded)
	}
	kept := source.refs[:0:0]
	for _, ref := range source.refs {
		if ref.Key != superseded[0] {
			kept = append(kept, ref)
		}
	}
	source.refs = kept

	// Inside the window: resolves.
	if _, err := SelectAtOrBefore(ctx, source, now.AddDate(0, 0, -1)); err != nil {
		t.Errorf("a target inside the window should resolve: %v", err)
	}

	// Between the anchor and the window's start: resolves to the anchor, which
	// is correct — no configuration change happened in that span, which is
	// exactly why no snapshot sits there.
	resolution, err := SelectAtOrBefore(ctx, source, now.AddDate(0, 0, -100))
	if err != nil {
		t.Fatalf("a target the anchor covers should resolve: %v", err)
	}
	if got := resolution.Snapshot.Spec["instances"]; got != float64(300) {
		t.Errorf("selected the wrong snapshot: instances=%v, want the 300-day anchor", got)
	}

	// Older than the anchor: refused, naming what is actually available.
	_, err = SelectAtOrBefore(ctx, source, now.AddDate(0, 0, -350))
	if err == nil {
		t.Fatal("a target older than the retained history must be refused, not approximated")
	}
	if !strings.Contains(err.Error(), "does not reach back") {
		t.Errorf("the refusal should say the history does not reach back; got: %v", err)
	}
}
