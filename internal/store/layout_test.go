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
	"testing"
	"time"
)

func TestParseDestination(t *testing.T) {
	cases := []struct {
		in                     string
		scheme, bucket, prefix string
		wantErr                bool
	}{
		{in: "s3://backups/", scheme: "s3", bucket: "backups"},
		{in: "s3://backups", scheme: "s3", bucket: "backups"},
		{in: "s3://backups/nested/path", scheme: "s3", bucket: "backups", prefix: "nested/path"},
		{in: "gs://bucket/p", scheme: "gs", bucket: "bucket", prefix: "p"},
		{in: "azure://container", scheme: "azure", bucket: "container"},
		{in: "S3://Backups/", scheme: "s3", bucket: "Backups"},
		{in: "backups", wantErr: true},
		{in: "ftp://backups", wantErr: true},
		{in: "s3://", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tc := range cases {
		got, err := ParseDestination(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDestination(%q) = %+v; want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDestination(%q): %v", tc.in, err)
			continue
		}
		if got.Scheme != tc.scheme || got.Bucket != tc.bucket || got.Path != tc.prefix {
			t.Errorf("ParseDestination(%q) = %+v; want {%s %s %s}",
				tc.in, got, tc.scheme, tc.bucket, tc.prefix)
		}
	}
}

func layout() Layout {
	return Layout{ServerName: "pg-source", Prefix: "chronicle"}
}

func TestLayoutKeys(t *testing.T) {
	l := layout()
	if got, want := l.Root(), "pg-source/chronicle"; got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
	if got, want := l.SnapshotsPrefix(), "pg-source/chronicle/snapshots/"; got != want {
		t.Errorf("SnapshotsPrefix = %q, want %q", got, want)
	}
	if got, want := l.LatestKey(), "pg-source/chronicle/latest.json"; got != want {
		t.Errorf("LatestKey = %q, want %q", got, want)
	}

	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if got, want := l.SnapshotKey(at, 42, "sha256:3dab90cc6e2a"), "pg-source/chronicle/snapshots/20260825T120000Z-g0000000042-3dab90cc.json"; got != want {
		t.Errorf("SnapshotKey = %q, want %q", got, want)
	}
}

// Snapshots must never land under barman's directories: it enumerates those and
// would treat our objects as backups or WAL segments.
func TestLayoutStaysOutOfBarmanDirectories(t *testing.T) {
	l := layout()
	for _, key := range []string{l.Root(), l.SnapshotsPrefix(), l.LatestKey()} {
		for _, barman := range []string{"pg-source/base", "pg-source/wals"} {
			if len(key) >= len(barman) && key[:len(barman)] == barman {
				t.Errorf("key %q falls inside barman's %q", key, barman)
			}
		}
	}
}

func TestLayoutHonoursNestedDestinationPath(t *testing.T) {
	l := Layout{BasePath: "team/prod", ServerName: "pg-source", Prefix: "chronicle"}
	if got, want := l.Root(), "team/prod/pg-source/chronicle"; got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}

func TestParseSnapshotKeyRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 25, 12, 34, 56, 0, time.UTC)
	key := layout().SnapshotKey(at, 7, "sha256:abcdef01deadbeef")

	gotAt, gotGeneration, err := ParseSnapshotKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !gotAt.Equal(at) {
		t.Errorf("time = %v, want %v", gotAt, at)
	}
	if gotGeneration != 7 {
		t.Errorf("generation = %d, want 7", gotGeneration)
	}
}

func TestParseSnapshotKeyRejectsForeignObjects(t *testing.T) {
	// A LIST over our prefix may turn up anything a human dropped there; those
	// must be skipped rather than misread as snapshots.
	for _, key := range []string{
		"pg-source/chronicle/latest.json",
		"pg-source/chronicle/snapshots/README.md",
		"pg-source/chronicle/snapshots/20260825T120000Z.json",
		"pg-source/chronicle/snapshots/20260825T120000Z-g0000000001.json",
		"pg-source/chronicle/snapshots/notatimestamp-g0000000001-abcdef01.json",
	} {
		if _, _, err := ParseSnapshotKey(key); err == nil {
			t.Errorf("ParseSnapshotKey(%q) succeeded; want error", key)
		}
	}
}

// Lexical order over keys must equal chronological order, because point-in-time
// selection relies on scanning a sorted listing.
func TestKeysSortChronologically(t *testing.T) {
	l := layout()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	type entry struct {
		key string
		at  time.Time
	}
	var entries []entry
	for i, offset := range []time.Duration{
		0, 90 * time.Minute, 36 * time.Hour, 400 * 24 * time.Hour, 5 * time.Second,
	} {
		at := base.Add(offset)
		entries = append(entries, entry{key: l.SnapshotKey(at, int64(i+1), "sha256:0000000a"), at: at})
	}

	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.key
	}
	sort.Strings(keys)

	var previous time.Time
	for _, key := range keys {
		at, _, err := ParseSnapshotKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(previous) {
			t.Fatalf("lexical order is not chronological: %v came after %v", at, previous)
		}
		previous = at
	}
}

// Two captures inside the same second are ordered by generation.
func TestSameSecondTiesBreakByGeneration(t *testing.T) {
	l := layout()
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first, second := l.SnapshotKey(at, 9, "sha256:ffffffff"), l.SnapshotKey(at, 10, "sha256:00000000")
	if !(first < second) {
		t.Errorf("generation 9 key %q should sort before generation 10 key %q", first, second)
	}
}

// Two captures of the same generation inside the same second are distinct
// events (a label edit does not bump generation), so they must not collide.
func TestSameSecondSameGenerationDifferentContentDoesNotCollide(t *testing.T) {
	l := layout()
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	first := l.SnapshotKey(at, 7, "sha256:aaaaaaaabbbb")
	second := l.SnapshotKey(at, 7, "sha256:ccccccccdddd")
	if first == second {
		t.Fatalf("identical key for different content: %q", first)
	}

	// Identical content must still map to one key, so a retry overwrites in
	// place rather than accumulating duplicates.
	if again := l.SnapshotKey(at, 7, "sha256:aaaaaaaabbbb"); again != first {
		t.Errorf("same content produced two keys: %q and %q", first, again)
	}
}

func TestShortHashHandlesMalformedChecksums(t *testing.T) {
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, checksum := range []string{"", "sha256:", "abc", "sha256:12345678"} {
		key := layout().SnapshotKey(at, 1, checksum)
		if _, _, err := ParseSnapshotKey(key); err != nil {
			t.Errorf("checksum %q produced an unparseable key %q: %v", checksum, key, err)
		}
	}
}
