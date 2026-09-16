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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

// stubReader answers every Get with a fixed error, standing in for an API
// server that has never heard of the barman ObjectStore CRD.
type stubReader struct{ err error }

func (s stubReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return s.err
}

func (s stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return s.err
}

func barmanObjectStore(name string, configuration map[string]any) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(BarmanObjectStoreGVK)
	object.SetName(name)
	object.SetNamespace("default")
	_ = unstructured.SetNestedMap(object.Object, configuration, "spec", "configuration")
	return object
}

func readerWith(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(BarmanObjectStoreGVK, &unstructured.Unstructured{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func inlineStore() *chroniclev1.ConfigStore {
	return &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "config-store", Namespace: "default"},
		Spec: chroniclev1.ConfigStoreSpec{
			Configuration: &chroniclev1.ObjectStoreConfiguration{
				DestinationPath: "s3://backups/nested",
				EndpointURL:     "http://objectstore:9000",
				S3Credentials:   &chroniclev1.S3Credentials{InheritFromIAMRole: true},
			},
		},
	}
}

func TestResolveInlineConfiguration(t *testing.T) {
	resolver := &Resolver{}
	got, err := resolver.Resolve(context.Background(), inlineStore())
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "Inline" {
		t.Errorf("source = %q, want Inline", got.Source)
	}
	if got.Destination.Bucket != "backups" || got.Destination.Path != "nested" {
		t.Errorf("destination = %+v", got.Destination)
	}
	if got.Provider != "s3" {
		t.Errorf("provider = %q", got.Provider)
	}
}

func TestResolveDerivedFromBarmanObjectStore(t *testing.T) {
	reader := readerWith(t, barmanObjectStore("backup-store", map[string]any{
		"destinationPath": "s3://backups/",
		"endpointURL":     "http://backups:9000",
		// Fields this plugin has no use for; they must not survive.
		"wal":  map[string]any{"compression": "gzip"},
		"tags": map[string]any{"team": "platform"},
	}))

	resolver := &Resolver{ObjectStores: NewObjectStoreReader(reader, DefaultObjectStoreTTL)}
	configStore := &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "meta-derived", Namespace: "default"},
		Spec: chroniclev1.ConfigStoreSpec{
			DerivedFrom: &chroniclev1.BarmanObjectStoreRef{Name: "backup-store"},
		},
	}

	got, err := resolver.Resolve(context.Background(), configStore)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "ObjectStore/backup-store" {
		t.Errorf("source = %q", got.Source)
	}
	if got.Configuration.DestinationPath != "s3://backups/" {
		t.Errorf("destinationPath = %q", got.Configuration.DestinationPath)
	}
}

// The CRD being absent is the case the whole uncached-read design exists for:
// it must be a legible error on one store, never a startup failure.
func TestResolveReportsAbsentBarmanCRD(t *testing.T) {
	reader := stubReader{err: &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: BarmanObjectStoreGVK.Group, Kind: "ObjectStore"},
	}}
	resolver := &Resolver{ObjectStores: NewObjectStoreReader(reader, DefaultObjectStoreTTL)}

	_, err := resolver.Resolve(context.Background(), &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "meta-derived", Namespace: "default"},
		Spec: chroniclev1.ConfigStoreSpec{
			DerivedFrom: &chroniclev1.BarmanObjectStoreRef{Name: "backup-store"},
		},
	})

	var absent *ObjectStoreCRDAbsentError
	if !errors.As(err, &absent) {
		t.Fatalf("err = %v, want ObjectStoreCRDAbsentError", err)
	}
	if !strings.Contains(absent.Error(), "barman-cloud plugin") {
		t.Errorf("the error should say how to fix it: %v", absent)
	}
	if absent.Unwrap() == nil {
		t.Error("the cause should be unwrappable")
	}
}

func TestResolveRejectsStoreWithNeitherSource(t *testing.T) {
	resolver := &Resolver{}
	_, err := resolver.Resolve(context.Background(), &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
	})
	if err == nil || !strings.Contains(err.Error(), "neither derivedFrom nor configuration") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRejectsUnparseableDestination(t *testing.T) {
	configStore := inlineStore()
	configStore.Spec.Configuration.DestinationPath = "not-a-url"

	if _, err := (&Resolver{}).Resolve(context.Background(), configStore); err == nil {
		t.Fatal("a destinationPath with no scheme should be rejected")
	}
}

// A second read inside the TTL must not hit the API server again; the reader is
// uncached, and the hook runs on every reconciliation.
func TestObjectStoreReaderCachesWithinTTL(t *testing.T) {
	reader := &countingReader{
		inner: readerWith(t, barmanObjectStore("backup-store", map[string]any{
			"destinationPath": "s3://backups/",
		})),
	}
	objectStores := NewObjectStoreReader(reader, time.Minute)
	key := types.NamespacedName{Namespace: "default", Name: "backup-store"}

	for i := 0; i < 3; i++ {
		if _, err := objectStores.GetConfiguration(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if reader.gets != 1 {
		t.Errorf("hit the API server %d times, want 1", reader.gets)
	}
}

func TestObjectStoreReaderExpiresCache(t *testing.T) {
	reader := &countingReader{
		inner: readerWith(t, barmanObjectStore("backup-store", map[string]any{
			"destinationPath": "s3://backups/",
		})),
	}
	objectStores := NewObjectStoreReader(reader, time.Minute)
	key := types.NamespacedName{Namespace: "default", Name: "backup-store"}

	now := time.Now()
	objectStores.now = func() time.Time { return now }
	if _, err := objectStores.GetConfiguration(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	objectStores.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := objectStores.GetConfiguration(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if reader.gets != 2 {
		t.Errorf("hit the API server %d times, want 2 after expiry", reader.gets)
	}
}

func TestObjectStoreReaderReportsMissingStore(t *testing.T) {
	objectStores := NewObjectStoreReader(readerWith(t), DefaultObjectStoreTTL)
	_, err := objectStores.GetConfiguration(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "absent"})
	if !apierrs.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

// countingReader records how many Gets reach the API server.
type countingReader struct {
	inner client.Reader
	gets  int
}

func (c *countingReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	c.gets++
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(
	ctx context.Context, list client.ObjectList, opts ...client.ListOption,
) error {
	return c.inner.List(ctx, list, opts...)
}
