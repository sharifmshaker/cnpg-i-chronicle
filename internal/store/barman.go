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
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

// BarmanObjectStoreGVK identifies the CR a ConfigStore can derive from.
var BarmanObjectStoreGVK = schema.GroupVersionKind{
	Group:   "barmancloud.cnpg.io",
	Version: "v1",
	Kind:    "ObjectStore",
}

// ObjectStoreCRDAbsentError means the barman-cloud ObjectStore CRD is not
// installed, so a derivedFrom reference cannot be resolved.
type ObjectStoreCRDAbsentError struct{ Err error }

func (e *ObjectStoreCRDAbsentError) Error() string {
	return fmt.Sprintf("the barmancloud.cnpg.io/v1 ObjectStore CRD is not installed, "+
		"so spec.derivedFrom cannot be resolved; install the barman-cloud plugin or use "+
		"spec.configuration instead: %v", e.Err)
}

func (e *ObjectStoreCRDAbsentError) Unwrap() error { return e.Err }

// DefaultObjectStoreTTL is how long a resolved ObjectStore is reused.
const DefaultObjectStoreTTL = 10 * time.Second

type cacheEntry struct {
	derived   *DerivedObjectStore
	expiresAt time.Time
}

// ObjectStoreReader reads barman-cloud ObjectStore resources.
//
// Two decisions here are load-bearing.
//
// First, it reads through an *uncached* client. controller-runtime cannot start
// an informer for a CRD that is not installed, and it fails at manager startup
// when it tries. Watching barmancloud.cnpg.io would therefore make the whole
// plugin refuse to run on a cluster that has no barman-cloud plugin — exactly
// the deployment the standalone spec.configuration path exists to support. A
// missing CRD has to surface as an ordinary error on one ConfigStore, not as
// a crash loop.
//
// Second, it reads the CR as unstructured rather than importing
// plugin-barman-cloud's Go types. Only .spec.configuration is needed, and its
// schema already comes from the shared barman-cloud module, so the typed import
// would buy nothing but a dependency on another plugin's release cadence.
//
// The TTL cache exists because uncached reads hit the API server directly and
// the reconciler hook runs on every Cluster reconciliation.
type ObjectStoreReader struct {
	reader client.Reader
	ttl    time.Duration

	mutex sync.Mutex
	cache map[types.NamespacedName]cacheEntry

	// now is overridable in tests.
	now func() time.Time
}

// NewObjectStoreReader builds a reader over an uncached client.Reader. Pass the
// manager's APIReader, never its cached client.
func NewObjectStoreReader(reader client.Reader, ttl time.Duration) *ObjectStoreReader {
	if ttl <= 0 {
		ttl = DefaultObjectStoreTTL
	}
	return &ObjectStoreReader{
		reader: reader,
		ttl:    ttl,
		cache:  map[types.NamespacedName]cacheEntry{},
		now:    time.Now,
	}
}

// DerivedObjectStore is what this plugin reads out of a barman ObjectStore.
type DerivedObjectStore struct {
	// Configuration is the narrowed object store configuration.
	Configuration *chroniclev1.ObjectStoreConfiguration

	// RetentionPolicy is barman's own retentionPolicy verbatim, or empty when
	// it sets none. Carried so a ConfigStore can follow it; barman remains
	// the authority on what it means for backups.
	RetentionPolicy string
}

// GetConfiguration returns the barman object store configuration named by key.
func (r *ObjectStoreReader) GetConfiguration(
	ctx context.Context,
	key types.NamespacedName,
) (*DerivedObjectStore, error) {
	if cached, ok := r.lookup(key); ok {
		return cached, nil
	}

	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(BarmanObjectStoreGVK)

	if err := r.reader.Get(ctx, key, object); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, &ObjectStoreCRDAbsentError{Err: err}
		}
		return nil, err
	}

	raw, found, err := unstructured.NestedMap(object.Object, "spec", "configuration")
	if err != nil {
		return nil, fmt.Errorf("while reading ObjectStore %s spec.configuration: %w", key, err)
	}
	if !found {
		return nil, fmt.Errorf("ObjectStore %s has no spec.configuration", key)
	}

	configuration, err := deriveConfiguration(raw, key)
	if err != nil {
		return nil, err
	}

	// Optional on barman's side, so its absence is not an error here.
	retention, _, err := unstructured.NestedString(object.Object, "spec", "retentionPolicy")
	if err != nil {
		return nil, fmt.Errorf("while reading ObjectStore %s spec.retentionPolicy: %w", key, err)
	}

	derived := &DerivedObjectStore{Configuration: configuration, RetentionPolicy: retention}
	r.store(key, derived)
	return derived, nil
}

// deriveConfiguration narrows a barman ObjectStore's configuration to the parts
// this plugin uses.
//
// The projection is explicit rather than a struct embed. barman's configuration
// also carries WAL and base-backup compression, encryption, parallelism, command
// arguments and object tags, none of which mean anything for writing a JSON
// document; carrying them would put settings in our own CRD that are read by
// nothing. Decoding into the narrow type drops them by construction.
//
// A provider this plugin cannot talk to is reported here rather than several
// layers down, so the ConfigStore says why it is not Ready instead of failing
// when a snapshot is first written.
func deriveConfiguration(
	raw map[string]any,
	key types.NamespacedName,
) (*chroniclev1.ObjectStoreConfiguration, error) {
	for _, unsupported := range []struct{ field, provider string }{
		{"azureCredentials", "Azure Blob Storage"},
		{"googleCredentials", "Google Cloud Storage"},
	} {
		if _, present := raw[unsupported.field]; present {
			return nil, fmt.Errorf(
				"ObjectStore %s uses %s, which this plugin does not support yet; "+
					"only S3-compatible object stores are implemented", key, unsupported.provider)
		}
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("while re-encoding ObjectStore %s configuration: %w", key, err)
	}

	configuration := &chroniclev1.ObjectStoreConfiguration{}
	if err := json.Unmarshal(encoded, configuration); err != nil {
		return nil, fmt.Errorf("while decoding ObjectStore %s configuration: %w", key, err)
	}

	if configuration.DestinationPath == "" {
		return nil, fmt.Errorf("ObjectStore %s has no destinationPath", key)
	}
	return configuration, nil
}

func (r *ObjectStoreReader) lookup(key types.NamespacedName) (*DerivedObjectStore, bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	entry, ok := r.cache[key]
	if !ok || r.now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.derived, true
}

func (r *ObjectStoreReader) store(key types.NamespacedName, derived *DerivedObjectStore) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.cache[key] = cacheEntry{derived: derived, expiresAt: r.now().Add(r.ttl)}
}
