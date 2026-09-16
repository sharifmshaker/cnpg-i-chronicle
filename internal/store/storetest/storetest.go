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

// Package storetest provides in-memory stand-ins for the object store, for
// tests in any package.
//
// There is one fake, and it follows the store.Backend contract rather than
// approximating it: listing is in lexical order and a missing object wraps
// store.ErrNotFound. Restore selection depends on both, so a fake that got
// either subtly wrong would let a test pass for a reason production does not
// share. Only tests import this package, so it is never linked into the
// plugin.
package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// Backend is an in-memory store.Backend. Build one with NewBackend.
type Backend struct {
	mutex sync.Mutex

	// Objects holds every object by key. A test may read or edit it directly
	// between calls, to plant a stray file or corrupt a snapshot.
	Objects map[string][]byte

	// Gets and Puts count calls, so a test can assert that a path reached the
	// store, or that it did not.
	Gets, Puts int

	// CheckErr is what Check returns.
	CheckErr error
}

var _ store.Backend = (*Backend)(nil)

// NewBackend returns an empty Backend.
func NewBackend() *Backend {
	return &Backend{Objects: map[string][]byte{}}
}

// Get returns a copy of the object, or an error wrapping store.ErrNotFound.
func (b *Backend) Get(_ context.Context, key string) ([]byte, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.Gets++
	data, ok := b.Objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	return slices.Clone(data), nil
}

// Put stores a copy of data at key.
func (b *Backend) Put(_ context.Context, key string, data []byte) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.Puts++
	b.Objects[key] = slices.Clone(data)
	return nil
}

// List returns every key under prefix, in lexical order.
func (b *Backend) List(_ context.Context, prefix string) ([]string, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	var keys []string
	for key := range b.Objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// Check returns CheckErr.
func (b *Backend) Check(context.Context, string) error {
	return b.CheckErr
}

// Delete removes the object at key; a missing one is not an error.
func (b *Backend) Delete(_ context.Context, key string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.Objects, key)
	return nil
}

// Provider resolves every ConfigStore to one inline S3 configuration backed by
// Store, or fails with the error set for that step.
type Provider struct {
	Store      store.Backend
	ResolveErr error
	BackendErr error
}

var _ store.Provider = Provider{}

// Resolve returns a fixed inline configuration for s3://backups/.
func (p Provider) Resolve(context.Context, *chroniclev1.ConfigStore) (*store.Resolved, error) {
	if p.ResolveErr != nil {
		return nil, p.ResolveErr
	}
	return &store.Resolved{
		Configuration: &chroniclev1.ObjectStoreConfiguration{
			DestinationPath: "s3://backups/",
			EndpointURL:     "http://objectstore:9000",
		},
		Destination: store.Destination{Scheme: "s3", Bucket: "backups"},
		Source:      "Inline",
		Provider:    "s3",
	}, nil
}

// Backend returns Store.
func (p Provider) Backend(context.Context, string, *store.Resolved) (store.Backend, error) {
	if p.BackendErr != nil {
		return nil, p.BackendErr
	}
	return p.Store, nil
}

// SeedSnapshot writes a snapshot of the given spec, written as JSON, into one
// server's history under the default "chronicle" prefix, exactly as a capture
// would: checksummed, and with the latest pointer updated.
func SeedSnapshot(
	t testing.TB,
	backend store.Backend,
	serverName string,
	generation int64,
	at time.Time,
	specJSON string,
) {
	t.Helper()

	spec, err := pathutil.FromJSON([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	content := snapshot.Content{Spec: spec}
	checksum, err := snapshot.Checksum(content)
	if err != nil {
		t.Fatal(err)
	}

	history := &store.SnapshotStore{
		Backend: backend,
		Layout:  store.Layout{ServerName: serverName, Prefix: chroniclev1.DefaultPrefix},
	}
	if _, err := history.Write(context.Background(), &snapshot.Snapshot{
		APIVersion: snapshot.APIVersion,
		Kind:       snapshot.Kind,
		CapturedAt: at,
		Generation: generation,
		Cluster:    snapshot.ClusterRef{Name: serverName, Namespace: "default"},
		Content:    content,
		Checksum:   checksum,
	}); err != nil {
		t.Fatal(err)
	}
}
