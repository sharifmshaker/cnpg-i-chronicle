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
	"sync"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/types"
)

// settledCache remembers cluster states the capture hook has confirmed against
// the object store, and which snapshot that confirmation found newest.
//
// It exists for one case, which is common: a generation bump that changes
// nothing this plugin captures. The watermark cannot absorb it — the watermark
// is the newest snapshot's own generation, and no snapshot was written — so
// without this both hooks would go back to the store to reach the same
// conclusion on every call. For the status hook, which CloudNativePG polls every
// five seconds, that would be a client build and a read per cluster per poll
// for as long as the cluster sits there.
//
// The two hooks share one cache. Capture fills it; status reads it, and trusts
// it only while the watermark already on the Cluster names the same snapshot,
// so a capture that wrote a new one is still published.
//
// Nothing depends on it for correctness. A miss costs one extract and one read,
// and the checksum comparison against the store is what actually decides. That
// is what lets it be dropped wholesale rather than carefully invalidated, and
// why losing it on restart is uninteresting.
type settledCache struct {
	mutex   sync.Mutex
	entries map[types.UID]settledEntry
}

type settledEntry struct {
	generation       int64
	metadataChecksum string
	snapshotKey      string
}

// maxSettledEntries bounds the map. Entries are keyed by cluster UID, so a
// cluster that is deleted and recreated leaves one behind; over a long-lived
// process those would accumulate. Clearing wholesale at a threshold costs one
// extra check per cluster afterwards, which is cheaper than tracking liveness
// for a cache that is only ever an optimisation.
const maxSettledEntries = 1024

func newSettledCache() *settledCache {
	return &settledCache{entries: map[types.UID]settledEntry{}}
}

// settled reports whether this exact cluster state was confirmed as captured,
// and the key of the snapshot that holds it.
func (c *settledCache) settled(cluster *apiv1.Cluster, metadataChecksum string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()

	entry, ok := c.entries[cluster.UID]
	if !ok || entry.generation != cluster.Generation || entry.metadataChecksum != metadataChecksum {
		return "", false
	}
	return entry.snapshotKey, true
}

// remember records that this cluster state is captured by the snapshot at key.
func (c *settledCache) remember(cluster *apiv1.Cluster, metadataChecksum, snapshotKey string) {
	if c == nil {
		return
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if len(c.entries) >= maxSettledEntries {
		c.entries = map[types.UID]settledEntry{}
	}
	c.entries[cluster.UID] = settledEntry{
		generation:       cluster.Generation,
		metadataChecksum: metadataChecksum,
		snapshotKey:      snapshotKey,
	}
}
