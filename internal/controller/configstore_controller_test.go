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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store/storetest"
)

// emptyStore is a ConfigStore with nothing but its identity.
func emptyStore() *chroniclev1.ConfigStore {
	return &chroniclev1.ConfigStore{
		ObjectMeta: metav1.ObjectMeta{Name: "config-store", Namespace: "default"},
	}
}

func storeWithRetention(retention string) *chroniclev1.ConfigStore {
	configStore := emptyStore()
	configStore.Spec.Retention = retention
	return configStore
}

func newReconciler(t *testing.T, objects ...client.Object) (*ConfigStoreReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		chroniclev1.AddToScheme, corev1.AddToScheme, apiv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&chroniclev1.ConfigStore{}, &chroniclev1.RestorePolicy{}).
		Build()
	return &ConfigStoreReconciler{Client: kube}, kube
}

// clusterUsing builds a Cluster that archives into a store, or into none.
func clusterUsing(name, storeName string) *apiv1.Cluster {
	cluster := &apiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if storeName != "" {
		cluster.Spec.Plugins = []apiv1.PluginConfiguration{{
			Name:       metadata.PluginName,
			Parameters: map[string]string{metadata.ParameterSaveToStore: storeName},
		}}
	}
	return cluster
}

// seedRefs writes empty snapshot objects at the given ages, oldest first.
func seedRefs(t *testing.T, backend *storetest.Backend, serverName string, daysAgo ...int) []string {
	t.Helper()
	keys := make([]string, 0, len(daysAgo))
	for i, d := range daysAgo {
		at := time.Now().UTC().AddDate(0, 0, -d)
		key := fmt.Sprintf("%s/chronicle/snapshots/%s-g%010d-abcd1234.json",
			serverName, at.Format("20060102T150405Z"), i+1)
		if err := backend.Put(context.Background(), key, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys
}

// Retention discovers histories from the Clusters that archive into the store.
//
// This is the case the whole retention design turns on: a cluster whose
// configuration has not changed in a year has one snapshot, a year old, and it
// describes what runs today.
func TestRetentionKeepsTheAnchorForAnUnchangedCluster(t *testing.T) {
	ctx := context.Background()
	backend := storetest.NewBackend()
	keys := seedRefs(t, backend, "pg-source", 365)

	reconciler, _ := newReconciler(t, storeWithRetention("30d"),
		clusterUsing("pg-source", "config-store"))
	reconciler.Resolver = storetest.Provider{Store: backend}

	if _, err := reconciler.Reconcile(ctx, storeRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, keys[0]); err != nil {
		t.Errorf("the only snapshot was deleted; it is the anchor for every target: %v", err)
	}
}

func TestRetentionDeletesSupersededHistory(t *testing.T) {
	ctx := context.Background()
	backend := storetest.NewBackend()
	keys := seedRefs(t, backend, "pg-source", 400, 300, 5)

	reconciler, _ := newReconciler(t, storeWithRetention("30d"),
		clusterUsing("pg-source", "config-store"))
	reconciler.Resolver = storetest.Provider{Store: backend}

	if _, err := reconciler.Reconcile(ctx, storeRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, keys[0]); err == nil {
		t.Error("the 400-day-old snapshot is superseded by the 300-day anchor and should be gone")
	}
	for _, keep := range keys[1:] {
		if _, err := backend.Get(ctx, keep); err != nil {
			t.Errorf("%s should have been kept: %v", keep, err)
		}
	}
}

// A cluster that does not archive here must not have its history pruned by this
// store's retention, even though the snapshots sit in the same bucket.
func TestRetentionIgnoresClustersArchivingElsewhere(t *testing.T) {
	ctx := context.Background()
	backend := storetest.NewBackend()
	keys := seedRefs(t, backend, "pg-elsewhere", 400, 300, 5)

	reconciler, _ := newReconciler(t, storeWithRetention("30d"),
		clusterUsing("pg-elsewhere", "some-other-store"))
	reconciler.Resolver = storetest.Provider{Store: backend}

	if _, err := reconciler.Reconcile(ctx, storeRequest()); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if _, err := backend.Get(ctx, key); err != nil {
			t.Errorf("%s was pruned, but that cluster archives to a different store", key)
		}
	}
}

func TestRetentionDefaultsToForever(t *testing.T) {
	ctx := context.Background()
	backend := storetest.NewBackend()
	keys := seedRefs(t, backend, "pg-source", 900, 800, 700)

	reconciler, kube := newReconciler(t, emptyStore(), clusterUsing("pg-source", "config-store"))
	reconciler.Resolver = storetest.Provider{Store: backend}

	if _, err := reconciler.Reconcile(ctx, storeRequest()); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if _, err := backend.Get(ctx, key); err != nil {
			t.Errorf("an unset retention must delete nothing, but %s is gone", key)
		}
	}

	var got chroniclev1.ConfigStore
	if err := kube.Get(ctx, storeRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Resolved == nil || got.Status.Resolved.Retention != chroniclev1.RetentionForever {
		t.Errorf("status should report Forever, got %+v", got.Status.Resolved)
	}
}

func TestRetentionInheritWithoutAPolicyIsNotReady(t *testing.T) {
	ctx := context.Background()
	reconciler, kube := newReconciler(t, storeWithRetention(chroniclev1.RetentionInherit))
	reconciler.Resolver = storetest.Provider{Store: storetest.NewBackend()}

	if _, err := reconciler.Reconcile(ctx, storeRequest()); err != nil {
		t.Fatalf("this belongs in status, not an error: %v", err)
	}

	var got chroniclev1.ConfigStore
	if err := kube.Get(ctx, storeRequest().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	ready := condition(got.Status.Conditions, chroniclev1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	if !strings.Contains(ready.Message, "retentionPolicy") {
		t.Errorf("message should say the object store sets none; got: %s", ready.Message)
	}
}
