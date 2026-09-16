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
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

// These tests run against a real S3-compatible server. They are skipped unless
// CHRONICLE_S3_ENDPOINT is set, so `go test ./...` stays hermetic:
//
//	docker run -d --name minio -p 19000:9000 \
//	  -e MINIO_ROOT_USER=chronicle -e MINIO_ROOT_PASSWORD=chronicle123 \
//	  quay.io/minio/minio:latest server /data
//	CHRONICLE_S3_ENDPOINT=http://localhost:19000 \
//	CHRONICLE_S3_ACCESS_KEY=chronicle \
//	CHRONICLE_S3_SECRET_KEY=chronicle123 \
//	CHRONICLE_S3_BUCKET=chronicle-test \
//	  go test ./internal/store/ -run Integration -v
//
// The same test is what proves the "any S3-compatible endpoint" claim: point it
// at MinIO, RustFS or real S3 and the assertions do not change.
func integrationBackend(t *testing.T) Backend {
	t.Helper()

	endpoint := os.Getenv("CHRONICLE_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("CHRONICLE_S3_ENDPOINT is not set; skipping S3 integration test")
	}

	bucket := os.Getenv("CHRONICLE_S3_BUCKET")
	if bucket == "" {
		bucket = "chronicle-test"
	}

	backend, err := NewS3Backend(context.Background(), S3Options{
		Bucket:          bucket,
		EndpointURL:     endpoint,
		AccessKeyID:     os.Getenv("CHRONICLE_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CHRONICLE_S3_SECRET_KEY"),
		Region:          os.Getenv("CHRONICLE_S3_REGION"),
	})
	if err != nil {
		t.Fatalf("building the S3 backend: %v", err)
	}
	return backend
}

func TestIntegrationS3RoundTrip(t *testing.T) {
	ctx := context.Background()
	backend := integrationBackend(t)

	key := "chronicle-itest/roundtrip.json"
	payload := []byte(`{"hello":"world"}`)

	if err := backend.Put(ctx, key, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = backend.Delete(ctx, key) })

	got, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}
}

// A missing key must map onto ErrNotFound rather than an opaque provider error:
// Latest() relies on telling "no snapshots yet" apart from "the store is broken".
func TestIntegrationS3MissingKeyIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	backend := integrationBackend(t)

	_, err := backend.Get(ctx, "chronicle-itest/definitely-absent.json")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestIntegrationS3ListIsOrderedAndPrefixed(t *testing.T) {
	ctx := context.Background()
	backend := integrationBackend(t)

	keys := []string{
		"chronicle-itest/list/c.json",
		"chronicle-itest/list/a.json",
		"chronicle-itest/list/b.json",
		"chronicle-itest/other/z.json",
	}
	for _, key := range keys {
		if err := backend.Put(ctx, key, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Delete(ctx, key) })
	}

	got, err := backend.List(ctx, "chronicle-itest/list/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d keys, want 3 (the prefix must exclude other/): %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("List is not sorted: %v", got)
		}
	}
}

// The probe behind a ConfigStore's Ready condition must succeed against a real
// server, and must not mistake an empty prefix for a failure.
func TestIntegrationS3Check(t *testing.T) {
	backend := integrationBackend(t)
	if err := backend.Check(context.Background(), "chronicle-itest/nothing-here"); err != nil &&
		!errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("Check: %v", err)
	}
}

func TestIntegrationS3DeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	backend := integrationBackend(t)

	// Deleting something that is not there must not be an error, or pruning
	// would fail on any partially-cleaned store.
	if err := backend.Delete(ctx, "chronicle-itest/never-existed.json"); err != nil {
		t.Fatalf("Delete of a missing key returned %v, want nil", err)
	}
}

// The whole snapshot layer over a real server, not a fake: this is what proves
// the checksum survives a genuine S3 round trip.
func TestIntegrationSnapshotStore(t *testing.T) {
	ctx := context.Background()
	backend := integrationBackend(t)

	snapshotStore := &SnapshotStore{
		Backend: backend,
		Layout:  Layout{BasePath: "chronicle-itest", ServerName: "pg-itest", Prefix: "chronicle"},
	}
	t.Cleanup(func() {
		keys, _ := backend.List(ctx, "chronicle-itest/pg-itest/")
		for _, key := range keys {
			_ = backend.Delete(ctx, key)
		}
	})

	spec, err := pathutil.FromJSON([]byte(`{"instances":3,"storage":{"size":"10Gi"},"big":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	content := snapshot.Content{Spec: spec, Labels: map[string]string{"team": "platform"}}
	checksum, err := snapshot.Checksum(content)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		snap := &snapshot.Snapshot{
			APIVersion: snapshot.APIVersion,
			Kind:       snapshot.Kind,
			CapturedAt: base.Add(time.Duration(i) * time.Hour),
			Generation: i,
			Cluster:    snapshot.ClusterRef{Name: "pg-itest", Namespace: "default"},
			Content:    content,
			Checksum:   checksum,
		}
		if _, err := snapshotStore.Write(ctx, snap); err != nil {
			t.Fatalf("Write generation %d: %v", i, err)
		}
	}

	refs, err := snapshotStore.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("List returned %d snapshots, want 3", len(refs))
	}
	for i := 1; i < len(refs); i++ {
		if refs[i].CapturedAt.Before(refs[i-1].CapturedAt) {
			t.Error("listing is not chronological")
		}
	}

	latest, key, err := snapshotStore.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.Generation != 3 {
		t.Errorf("Latest generation = %d, want 3", latest.Generation)
	}
	if _, err := backend.Get(ctx, key); err != nil {
		t.Errorf("Latest reported key %q, which does not exist: %v", key, err)
	}
	if latest.Checksum != checksum {
		t.Errorf("checksum did not survive the round trip: %s != %s", latest.Checksum, checksum)
	}
}
