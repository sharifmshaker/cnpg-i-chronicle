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

// Package store resolves where snapshots live and reads and writes them.
package store

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimestampFormat is the compact UTC ISO-8601 form used in object keys, matching
// the convention barman-cloud uses for backup identifiers.
const TimestampFormat = "20060102T150405Z"

// generationDigits is the zero padding applied to the generation in a key.
//
// Keys sort lexicographically, and we rely on that order being chronological so
// that point-in-time selection is a scan rather than a full parse of every
// object. The timestamp is the primary sort key; the padded generation only
// breaks ties between two snapshots captured inside the same second. Ten digits
// covers ten billion spec changes to a single cluster.
const generationDigits = 10

// Keys carry a short content hash as well as the timestamp and generation.
//
// Without it, two captures of the same generation inside the same second
// produce an identical key and the second silently overwrites the first. That
// happens in practice: a label edit does not bump generation, so a spec change
// and a label change landing together are two distinct captures at one
// generation. Including the hash also makes writes content-addressed, so
// re-writing identical content is a harmless no-op rather than a new object,
// and it makes a snapshot's key a pure function of the snapshot itself, which
// is what lets Latest name the object it read without a second request.
var snapshotKeyPattern = regexp.MustCompile(`^(\d{8}T\d{6}Z)-g(\d{10})-[0-9a-f]{8}\.json$`)

// Layout computes object keys for one cluster's snapshots within a store.
//
// Snapshots sit at <destinationPath>/<serverName>/<prefix>/, deliberately beside
// barman-cloud's "base/" and "wals/" rather than inside either. barman only ever
// enumerates those two prefixes (CloudBackupCatalog.get_backup_list and
// get_wal_paths) and deletes by explicit key, so it will neither trip over our
// objects nor clean them up. Their lifecycle is ours.
//
// The bucket is not part of the layout: keys are relative to it, and the
// Backend is already bound to one.
type Layout struct {
	// BasePath is the key prefix within the bucket, possibly empty.
	BasePath string

	// ServerName is the per-cluster namespace within the store.
	ServerName string

	// Prefix is the plugin's directory under the server name.
	Prefix string
}

// Root is the key prefix holding everything for this cluster.
func (l Layout) Root() string {
	return joinKey(l.BasePath, l.ServerName, l.Prefix)
}

// SnapshotsPrefix is the key prefix holding the snapshot objects.
func (l Layout) SnapshotsPrefix() string {
	return joinKey(l.Root(), "snapshots") + "/"
}

// SnapshotKey is the object key for one capture. It is a pure function of the
// snapshot's own fields, so the key an object lives under can always be
// recovered from its content.
func (l Layout) SnapshotKey(capturedAt time.Time, generation int64, checksum string) string {
	return l.SnapshotsPrefix() + fmt.Sprintf("%s-g%0*d-%s.json",
		capturedAt.UTC().Format(TimestampFormat), generationDigits, generation,
		shortHash(checksum))
}

// LatestKey points at a copy of the most recent snapshot, so that restoring
// "the latest" is a single GET rather than a LIST plus a GET.
func (l Layout) LatestKey() string {
	return joinKey(l.Root(), "latest.json")
}

// shortHash reduces a "sha256:<hex>" checksum to the 8 hex characters used in
// object keys. Anything unparseable degrades to a fixed placeholder rather than
// producing an invalid key.
func shortHash(checksum string) string {
	_, hex, found := strings.Cut(checksum, ":")
	if !found {
		hex = checksum
	}
	if len(hex) < 8 {
		return "00000000"
	}
	return hex[:8]
}

// ParseSnapshotKey recovers the capture time and generation from a key, so that
// selection can order and filter objects without fetching them.
func ParseSnapshotKey(key string) (time.Time, int64, error) {
	matches := snapshotKeyPattern.FindStringSubmatch(path.Base(key))
	if matches == nil {
		return time.Time{}, 0, fmt.Errorf("%q is not a snapshot key", key)
	}

	capturedAt, err := time.Parse(TimestampFormat, matches[1])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("while parsing timestamp in %q: %w", key, err)
	}
	generation, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("while parsing generation in %q: %w", key, err)
	}
	return capturedAt.UTC(), generation, nil
}

// joinKey concatenates non-empty key segments with single slashes.
func joinKey(segments ...string) string {
	var kept []string
	for _, segment := range segments {
		segment = strings.Trim(segment, "/")
		if segment != "" {
			kept = append(kept, segment)
		}
	}
	return strings.Join(kept, "/")
}

// Destination is a parsed barman-style destinationPath.
type Destination struct {
	// Scheme is s3, azure or gs.
	Scheme string

	// Bucket is the bucket or container name.
	Bucket string

	// Path is the prefix within the bucket, possibly empty.
	Path string
}

// ParseDestination splits a barman destinationPath such as "s3://backups/pg"
// into its scheme, bucket and prefix.
func ParseDestination(destinationPath string) (Destination, error) {
	if destinationPath == "" {
		return Destination{}, fmt.Errorf("destinationPath is empty")
	}

	scheme, remainder, found := strings.Cut(destinationPath, "://")
	if !found {
		return Destination{}, fmt.Errorf(
			"destinationPath %q has no scheme; expected s3://, azure:// or gs://", destinationPath)
	}

	scheme = strings.ToLower(scheme)
	switch scheme {
	case "s3", "azure", "gs":
	default:
		return Destination{}, fmt.Errorf("unsupported destinationPath scheme %q", scheme)
	}

	remainder = strings.Trim(remainder, "/")
	if remainder == "" {
		return Destination{}, fmt.Errorf("destinationPath %q names no bucket", destinationPath)
	}

	bucket, prefix, _ := strings.Cut(remainder, "/")
	return Destination{Scheme: scheme, Bucket: bucket, Path: strings.Trim(prefix, "/")}, nil
}
