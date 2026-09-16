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

package operator

import (
	"fmt"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func settledCluster(uid string, generation int64) *apiv1.Cluster {
	return &apiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-source", Namespace: "default",
			UID: types.UID(uid), Generation: generation,
		},
	}
}

// The cache must key on the whole state, not just the cluster: a generation or
// metadata change has to miss, or a real edit would be skipped.
func TestSettledCacheMatchesOnTheWholeState(t *testing.T) {
	cache := newSettledCache()
	cluster := settledCluster("uid-1", 2)
	cache.remember(cluster, "sha256:aaa", "key-1")

	if key, ok := cache.settled(cluster, "sha256:aaa"); !ok || key != "key-1" {
		t.Errorf("the remembered state should hit with its snapshot key, got %q, %v", key, ok)
	}
	if _, ok := cache.settled(settledCluster("uid-1", 3), "sha256:aaa"); ok {
		t.Error("a generation change must miss")
	}
	if _, ok := cache.settled(cluster, "sha256:bbb"); ok {
		t.Error("a metadata change must miss")
	}
	if _, ok := cache.settled(settledCluster("uid-2", 2), "sha256:aaa"); ok {
		t.Error("a different cluster must miss")
	}
}

// Keyed by UID rather than name, so a deleted and recreated cluster of the same
// name does not inherit the old one's verdict.
func TestSettledCacheKeysOnUID(t *testing.T) {
	cache := newSettledCache()
	cache.remember(settledCluster("uid-old", 1), "sha256:aaa", "key-1")

	if _, ok := cache.settled(settledCluster("uid-new", 1), "sha256:aaa"); ok {
		t.Error("a recreated cluster shares its name but not its history")
	}
}

// The map is bounded. Correctness never depends on a hit, so dropping
// everything at the threshold is a legitimate way to stay bounded.
func TestSettledCacheStaysBounded(t *testing.T) {
	cache := newSettledCache()
	for i := range maxSettledEntries + 10 {
		cache.remember(settledCluster(fmt.Sprintf("uid-%d", i), 1), "sha256:aaa", "key")
	}
	if len(cache.entries) > maxSettledEntries {
		t.Errorf("cache holds %d entries, over the %d bound", len(cache.entries), maxSettledEntries)
	}
}

// A nil cache is a valid configuration: it simply never hits.
func TestSettledCacheNilIsSafe(t *testing.T) {
	var cache *settledCache
	cache.remember(settledCluster("uid-1", 1), "sha256:aaa", "key")
	if _, ok := cache.settled(settledCluster("uid-1", 1), "sha256:aaa"); ok {
		t.Error("a nil cache must never report settled")
	}
}
