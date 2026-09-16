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

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

func testSnapshot(generation int64, at time.Time) *snapshot.Snapshot {
	spec, err := pathutil.FromJSON([]byte(`{
	  "instances": 3,
	  "storage": {"size": "10Gi"},
	  "resources": {"requests": {"memory": "2Gi"}},
	  "postgresql": {"parameters": {"shared_buffers": "128MB", "max_connections": "200"}}
	}`))
	if err != nil {
		panic(err)
	}

	content := snapshot.Content{
		Spec:   spec,
		Labels: map[string]string{"team": "platform"},
	}
	checksum, err := snapshot.Checksum(content)
	if err != nil {
		panic(err)
	}

	return &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion,
		Kind:       snapshot.Kind,
		CapturedAt: at,
		Generation: generation,
		Cluster:    snapshot.ClusterRef{Name: "pg-source", Namespace: "default"},
		Groups:     snapshot.DefaultGroupNames(),
		Content:    content,
		Checksum:   checksum,
	}
}

func testStore() (*store.SnapshotStore, *storetest.Backend) {
	backend := storetest.NewBackend()
	return &store.SnapshotStore{
		Backend: backend,
		Layout:  store.Layout{ServerName: "pg-source", Prefix: "chronicle"},
	}, backend
}

// A snapshot is replayed onto a live cluster spec, so it has to survive the
// round trip through the object store byte-for-byte, checksum included.
func TestWriteThenReadPreservesChecksum(t *testing.T) {
	ctx := context.Background()
	history, _ := testStore()

	original := testSnapshot(42, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	key, err := history.Write(ctx, original)
	if err != nil {
		t.Fatal(err)
	}

	got, err := history.Read(ctx, key)
	if err != nil {
		t.Fatalf("read back failed (a checksum mismatch here means encoding is not stable across the round trip): %v", err)
	}
	if got.Checksum != original.Checksum {
		t.Errorf("checksum = %s, want %s", got.Checksum, original.Checksum)
	}
	if got.Generation != 42 {
		t.Errorf("generation = %d, want 42", got.Generation)
	}
}

func TestReadRejectsCorruptedSnapshot(t *testing.T) {
	ctx := context.Background()
	history, backend := testStore()

	key, err := history.Write(ctx, testSnapshot(1, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}

	// Flip a stored value without updating the checksum.
	tampered := []byte(strings.Replace(string(backend.Objects[key]), `"10Gi"`, `"1Gi"`, 1))
	backend.Objects[key] = tampered

	if _, err := history.Read(ctx, key); err == nil {
		t.Fatal("Read accepted a snapshot whose contents no longer match its checksum")
	}
}

func TestReadRejectsForeignDocument(t *testing.T) {
	ctx := context.Background()
	history, backend := testStore()

	backend.Objects["pg-source/chronicle/snapshots/20260825T120000Z-g0000000001-abcdef01.json"] =
		[]byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)

	_, err := history.Read(ctx, "pg-source/chronicle/snapshots/20260825T120000Z-g0000000001-abcdef01.json")
	if err == nil {
		t.Fatal("Read accepted a document that is not a snapshot")
	}
}

func TestListIsChronologicalAndSkipsStrays(t *testing.T) {
	ctx := context.Background()
	history, backend := testStore()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{48 * time.Hour, 0, 24 * time.Hour} {
		if _, err := history.Write(ctx, testSnapshot(int64(i+1), base.Add(offset))); err != nil {
			t.Fatal(err)
		}
	}
	// Something a human dropped in the bucket.
	backend.Objects["pg-source/chronicle/snapshots/notes.txt"] = []byte("hello")

	refs, err := history.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("List returned %d refs, want 3 (strays must be skipped, not fatal)", len(refs))
	}
	for i := 1; i < len(refs); i++ {
		if refs[i].CapturedAt.Before(refs[i-1].CapturedAt) {
			t.Errorf("List is not chronological at index %d", i)
		}
	}
}

func TestLatestUsesPointerAndFallsBackToListing(t *testing.T) {
	ctx := context.Background()
	history, backend := testStore()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{0, 24 * time.Hour} {
		if _, err := history.Write(ctx, testSnapshot(int64(i+1), base.Add(offset))); err != nil {
			t.Fatal(err)
		}
	}

	latest, key, err := history.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Generation != 2 {
		t.Errorf("via pointer: generation = %d, want 2", latest.Generation)
	}
	// The pointer is a copy, so the key has to be recomputed — and it must
	// name the object that is really there.
	if _, err := backend.Get(ctx, key); err != nil {
		t.Errorf("Latest reported key %q, which does not exist: %v", key, err)
	}

	// A crash between the two writes leaves no pointer; listing must recover.
	delete(backend.Objects, history.Layout.LatestKey())
	latest, listedKey, err := history.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Generation != 2 {
		t.Errorf("via listing: generation = %d, want 2", latest.Generation)
	}
	if listedKey != key {
		t.Errorf("the two paths disagree on the key: %q via pointer, %q via listing", key, listedKey)
	}
}

func TestLatestOnEmptyStore(t *testing.T) {
	history, _ := testStore()
	_, _, err := history.Latest(context.Background())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Reading a snapshot must not depend on every number in it being small.
//
// encoding/json decodes into float64 by default, which re-encodes large
// integers in scientific notation and long decimals with different precision.
// The checksum is computed over that re-encoding, so a value that does not
// survive the float64 detour makes a perfectly good snapshot fail verification
// and the restore fail with it.
func TestReadPreservesNumberPrecision(t *testing.T) {
	ctx := context.Background()
	history, _ := testStore()

	spec, err := pathutil.FromJSON([]byte(`{
	  "instances": 3,
	  "bigNumber": 9007199254740993,
	  "preciseFloat": 0.30000000000000004
	}`))
	if err != nil {
		t.Fatal(err)
	}
	content := snapshot.Content{Spec: spec}
	checksum, err := snapshot.Checksum(content)
	if err != nil {
		t.Fatal(err)
	}

	snap := &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion,
		Kind:       snapshot.Kind,
		CapturedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Generation: 1,
		Content:    content,
		Checksum:   checksum,
	}

	key, err := history.Write(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := history.Read(ctx, key); err != nil {
		t.Fatalf("round trip lost number precision: %v", err)
	}
}

// A document with no checksum cannot be verified, and the plugin never writes
// one, so it is refused rather than replayed onto a cluster on trust.
func TestReadRejectsSnapshotWithoutChecksum(t *testing.T) {
	ctx := context.Background()
	history, backend := testStore()

	key, err := history.Write(ctx, testSnapshot(1, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	document, err := pathutil.FromJSON(backend.Objects[key])
	if err != nil {
		t.Fatal(err)
	}
	delete(document, "checksum")
	if backend.Objects[key], err = json.Marshal(document); err != nil {
		t.Fatal(err)
	}

	if _, err := history.Read(ctx, key); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("a snapshot with no checksum should be refused, got: %v", err)
	}
}
