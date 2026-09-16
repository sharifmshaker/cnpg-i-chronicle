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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

// SnapshotRef is a snapshot's identity as recoverable from its key alone,
// without fetching the object.
type SnapshotRef struct {
	Key        string
	CapturedAt time.Time
	Generation int64
}

// SnapshotStore reads and writes snapshots for one cluster.
type SnapshotStore struct {
	Backend Backend
	Layout  Layout
}

// Open resolves a ConfigStore and returns the snapshot store for one server
// name within it. It is the one place the capture hook, the status hook and the
// restore webhook all go through to reach a history.
func Open(
	ctx context.Context,
	provider Provider,
	configStore *chroniclev1.ConfigStore,
	serverName string,
) (*SnapshotStore, error) {
	resolved, err := provider.Resolve(ctx, configStore)
	if err != nil {
		return nil, err
	}
	backend, err := provider.Backend(ctx, configStore.Namespace, resolved)
	if err != nil {
		return nil, err
	}
	return &SnapshotStore{
		Backend: backend,
		Layout:  LayoutFor(configStore, resolved, serverName),
	}, nil
}

// Write stores a snapshot and refreshes the latest pointer.
//
// The snapshot object is written first. If the process dies between the two
// writes, latest.json lags by one capture, which selection can repair by
// listing; the reverse order would leave latest.json pointing at an object that
// does not exist.
func (s *SnapshotStore) Write(ctx context.Context, snap *snapshot.Snapshot) (string, error) {
	encoded, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", fmt.Errorf("while encoding snapshot: %w", err)
	}

	key := s.Layout.SnapshotKey(snap.CapturedAt, snap.Generation, snap.Checksum)
	if err := s.Backend.Put(ctx, key, encoded); err != nil {
		return "", err
	}
	if err := s.Backend.Put(ctx, s.Layout.LatestKey(), encoded); err != nil {
		return "", fmt.Errorf("while updating the latest pointer: %w", err)
	}
	return key, nil
}

// sortRefs puts snapshots in chronological order, totally.
//
// The key is the final tiebreak, and it has to be there. Two captures can share
// a second and a generation — which is precisely the case content-addressed
// keys were introduced for, since a label edit does not bump generation — and
// on (CapturedAt, Generation) alone those two compare equal. sort.Slice is not
// stable, so their relative order would then vary between calls, and selection
// walks this list to answer "the newest at or before T". Two runs could resolve
// the same restore to different snapshots.
//
// Keys are timestamp-prefixed and content-addressed, so ordering by key is
// chronological anyway and total by construction.
func sortRefs(refs []SnapshotRef) {
	sort.Slice(refs, func(i, j int) bool {
		if !refs[i].CapturedAt.Equal(refs[j].CapturedAt) {
			return refs[i].CapturedAt.Before(refs[j].CapturedAt)
		}
		if refs[i].Generation != refs[j].Generation {
			return refs[i].Generation < refs[j].Generation
		}
		return refs[i].Key < refs[j].Key
	})
}

// Read fetches and validates one snapshot by key.
func (s *SnapshotStore) Read(ctx context.Context, key string) (*snapshot.Snapshot, error) {
	raw, err := s.Backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return decodeSnapshot(raw, key)
}

// List enumerates the snapshots present, oldest first.
//
// Keys that do not parse are skipped rather than treated as errors: the prefix
// is in a bucket a human can write to, and one stray file should not make every
// restore fail.
func (s *SnapshotStore) List(ctx context.Context) ([]SnapshotRef, error) {
	keys, err := s.Backend.List(ctx, s.Layout.SnapshotsPrefix())
	if err != nil {
		return nil, err
	}

	refs := make([]SnapshotRef, 0, len(keys))
	for _, key := range keys {
		capturedAt, generation, err := ParseSnapshotKey(key)
		if err != nil {
			continue
		}
		refs = append(refs, SnapshotRef{Key: key, CapturedAt: capturedAt, Generation: generation})
	}

	sortRefs(refs)
	return refs, nil
}

// Latest returns the most recent snapshot and the key it lives under.
//
// It tries the latest pointer first, which is a single GET, and falls back to a
// listing when the pointer is missing. The pointer is a copy rather than the
// object itself, so the key is recomputed from the snapshot's own fields — which
// is exact, because SnapshotKey is a pure function of them.
func (s *SnapshotStore) Latest(ctx context.Context) (*snapshot.Snapshot, string, error) {
	raw, err := s.Backend.Get(ctx, s.Layout.LatestKey())
	if err == nil {
		snap, err := decodeSnapshot(raw, s.Layout.LatestKey())
		if err != nil {
			return nil, "", err
		}
		return snap, s.Layout.SnapshotKey(snap.CapturedAt, snap.Generation, snap.Checksum), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, "", err
	}

	refs, err := s.List(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(refs) == 0 {
		return nil, "", fmt.Errorf("%w: no snapshots under %s", ErrNotFound, s.Layout.SnapshotsPrefix())
	}
	key := refs[len(refs)-1].Key
	snap, err := s.Read(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return snap, key, nil
}

func decodeSnapshot(raw []byte, key string) (*snapshot.Snapshot, error) {
	var snap snapshot.Snapshot

	// UseNumber, not json.Unmarshal. The captured spec is map[string]any, and
	// the default decoder would turn every number in it into a float64. Large
	// integers then re-encode in scientific notation and long decimals lose
	// digits, so recomputing the checksum over the decoded value would disagree
	// with the one recorded at capture time and reject a perfectly good
	// snapshot. This mirrors pathutil.ToMap on the write side.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&snap); err != nil {
		return nil, fmt.Errorf("while decoding snapshot %s: %w", key, err)
	}

	if snap.Kind != snapshot.Kind {
		return nil, fmt.Errorf("object %s is not a %s (kind=%q)", key, snapshot.Kind, snap.Kind)
	}
	if snap.APIVersion != snapshot.APIVersion {
		return nil, fmt.Errorf(
			"snapshot %s has unsupported apiVersion %q; this plugin understands %q",
			key, snap.APIVersion, snapshot.APIVersion)
	}

	// A snapshot is replayed onto a live cluster's spec, so corruption in transit
	// must be caught here rather than surfacing as a puzzling restored value. A
	// document with no checksum at all cannot be verified and is refused for the
	// same reason: the plugin never writes one, so it was not written by the
	// plugin.
	if snap.Checksum == "" {
		return nil, fmt.Errorf("snapshot %s carries no checksum and cannot be verified", key)
	}
	computed, err := snapshot.Checksum(snap.Content)
	if err != nil {
		return nil, fmt.Errorf("while verifying snapshot %s: %w", key, err)
	}
	if computed != snap.Checksum {
		return nil, fmt.Errorf(
			"snapshot %s failed checksum verification: recorded %s, computed %s",
			key, snap.Checksum, computed)
	}
	return &snap, nil
}
