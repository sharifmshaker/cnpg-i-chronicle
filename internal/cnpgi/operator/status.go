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
	"context"
	"encoding/json"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/clusterstatus"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/decoder"
	"github.com/cloudnative-pg/cnpg-i/pkg/operator"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/operator/config"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// OperatorImplementation serves the CNPG-I Operator service.
//
// Only SetStatusInCluster is implemented. MutateCluster and the two validation
// RPCs are part of the protocol but have had no caller in CloudNativePG since
// remote plugin support replaced the unix-socket loader the admission webhook
// used to build, so implementing them would produce code nothing invokes.
type OperatorImplementation struct {
	operator.UnimplementedOperatorServer

	Client client.Client

	// Resolver reaches the object store, which is where the watermark this
	// publishes actually lives.
	Resolver store.Provider

	// Settled is shared with the capture hook, which fills it. Optional: a nil
	// cache simply never hits.
	Settled *settledCache
}

// ClusterStatus is what the plugin publishes into
// .status.pluginStatus[chronicle.sharifmshaker.github.io].status.
type ClusterStatus struct {
	// LastSavedGeneration is the Cluster generation most recently captured.
	LastSavedGeneration int64 `json:"lastSavedGeneration"`

	// LastSavedAt is when that capture happened.
	LastSavedAt string `json:"lastSavedAt,omitempty"`

	// SnapshotKey is the object key of the most recent snapshot.
	SnapshotKey string `json:"snapshotKey,omitempty"`

	// Checksum is the snapshot's own content hash, published for people rather
	// than for the plugin: it is the value to compare against the bucket when a
	// capture looks wrong. Nothing reads it back — the capture hook compares
	// against the checksum on the newest object in the store, never this one.
	Checksum string `json:"checksum,omitempty"`

	// MetadataChecksum is what the capture hook does compare against, together
	// with LastSavedGeneration. It is the reason this status is a watermark
	// rather than a display: the hook reads it straight out of the Cluster it
	// was handed, so the common case costs no reads at all.
	MetadataChecksum string `json:"metadataChecksum,omitempty"`

	// Store and ServerName say where this history lives.
	Store      string `json:"store,omitempty"`
	ServerName string `json:"serverName,omitempty"`
}

// WatermarkFromCluster reads the capture watermark this plugin last published.
//
// This is the cheapest path the capture hook has: the watermark arrives inside
// the Cluster the hook was already handed, so the common case — a reconcile of
// a state already captured — costs no reads at all.
//
// It returns false when there is nothing to read — the plugin has never
// reported, or the operator has not yet refreshed status after a capture. Both
// mean "assume nothing was captured", which costs a re-check against the object
// store and never skips a capture that should happen.
func WatermarkFromCluster(cluster *apiv1.Cluster) (ClusterStatus, bool) {
	for _, plugin := range cluster.Status.PluginStatus {
		if plugin.Name != metadata.PluginName || plugin.Status == "" {
			continue
		}
		var status ClusterStatus
		if err := json.Unmarshal([]byte(plugin.Status), &status); err != nil {
			// Malformed status is treated as absent rather than fatal: it can
			// only cost a redundant check, and failing here would wedge capture
			// on a value the plugin does not control.
			return ClusterStatus{}, false
		}
		return status, true
	}
	return ClusterStatus{}, false
}

// GetCapabilities declares the operator RPCs this plugin answers.
func (o OperatorImplementation) GetCapabilities(
	_ context.Context,
	_ *operator.OperatorCapabilitiesRequest,
) (*operator.OperatorCapabilitiesResult, error) {
	return &operator.OperatorCapabilitiesResult{
		Capabilities: []*operator.OperatorCapability{
			{
				Type: &operator.OperatorCapability_Rpc{
					Rpc: &operator.OperatorCapability_RPC{
						Type: operator.OperatorCapability_RPC_TYPE_SET_STATUS_IN_CLUSTER,
					},
				},
			},
		},
	}, nil
}

// SetStatusInCluster publishes the capture watermark onto the Cluster.
//
// Every value in the response comes from the object store, never from the wall
// clock and never from a value this process merely believes it wrote. Two
// things depend on that:
//
// CloudNativePG polls this every five seconds for as long as a plugin reports a
// status, and patches whatever comes back. A status that was not a pure
// function of observed state would rewrite the Cluster on every pass.
//
// And because the bucket is the only source, this status can under-report but
// never over-report. A missing or stale watermark costs one extra check on the
// next capture; a watermark claiming a snapshot the bucket does not hold would
// silently stop capture, which is the failure that matters.
//
// The one thing it does reuse is its own previous answer, and only when the
// Cluster proves it still holds — see the comment on that check below.
func (o OperatorImplementation) SetStatusInCluster(
	ctx context.Context,
	request *operator.SetStatusInClusterRequest,
) (*operator.SetStatusInClusterResponse, error) {
	cluster, err := decoder.DecodeClusterLenient(request.GetCluster())
	if err != nil {
		return nil, err
	}

	pluginConfiguration := config.NewFromCluster(cluster)
	if !pluginConfiguration.IsSaveEnabled() {
		return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
	}

	var configStore chroniclev1.ConfigStore
	if err := o.Client.Get(ctx, pluginConfiguration.SaveToKey(), &configStore); err != nil {
		if apierrs.IsNotFound(err) {
			return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
		}
		return nil, err
	}

	// CloudNativePG polls this every five seconds for as long as a plugin
	// reports a status, so the settled case has to be free. It is: the Cluster
	// handed to us already carries what we last published, and the two cheap
	// fields below say whether the store can have moved since. Deriving the same
	// answer again would cost two object GETs per cluster every five seconds and
	// arrive at a byte-identical status.
	//
	// Correctness rests on the same reasoning as the capture hook's fast path:
	// the newest snapshot is a function of generation and captured metadata, so
	// a Cluster still sitting on both is a Cluster whose watermark still holds.
	// Anything that could move the store — a spec change, a label edit, a
	// different destination — fails one of these comparisons and falls through
	// to the store below.
	//
	// The residual is an out-of-band edit of the bucket while the Cluster sits
	// unchanged, which is noticed at its next change rather than within five
	// seconds. The capture hook already trusts the watermark that way.
	//
	// A generation bump that changed nothing captured cannot be answered from
	// the watermark, whose generation is the older snapshot's. The capture hook
	// confirms those against the store and records them in the settled cache,
	// along with the snapshot it found. The watermark still holds when it names
	// that same snapshot; when it does not, the capture wrote something new and
	// the store has to be read to publish it.
	fingerprint, err := liveFingerprint(cluster, configStore.GetCaptureGroups())
	if err != nil {
		return nil, err
	}
	if published, ok := WatermarkFromCluster(cluster); ok &&
		published.Store == pluginConfiguration.SaveToStore &&
		published.ServerName == pluginConfiguration.SaveToServer {
		if published.LastSavedGeneration == cluster.Generation &&
			published.MetadataChecksum == fingerprint {
			return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
		}
		if key, settled := o.Settled.settled(cluster, fingerprint); settled && key == published.SnapshotKey {
			return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
		}
	}

	// From here on, anything that stops the bucket answering is a no-op, which
	// leaves whatever status the Cluster already carries untouched. An
	// unreachable store is not an empty one, and a store with no snapshots yet
	// is the normal state of a cluster that has just been created; neither is
	// worth failing a reconcile over, and neither should erase a watermark
	// that was true a moment ago.
	snapshots, err := store.Open(ctx, o.Resolver, &configStore, pluginConfiguration.SaveToServer)
	if err != nil {
		return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
	}
	latest, key, err := snapshots.Latest(ctx)
	if err != nil {
		return clusterstatus.NewSetStatusInClusterResponseBuilder().NoOpResponse(), nil
	}

	// Recomputed rather than stored: every input — labels, annotations, groups
	// and capture rules — is recorded in the snapshot itself.
	metadataChecksum, err := snapshot.MetadataFingerprint(
		latest.Labels, latest.Annotations, latest.Groups, latest.CaptureRules)
	if err != nil {
		return nil, err
	}

	return clusterstatus.NewSetStatusInClusterResponseBuilder().JSONStatusResponse(ClusterStatus{
		LastSavedGeneration: latest.Generation,
		LastSavedAt:         latest.CapturedAt.UTC().Format(time.RFC3339),
		SnapshotKey:         key,
		Checksum:            latest.Checksum,
		MetadataChecksum:    metadataChecksum,
		Store:               pluginConfiguration.SaveToStore,
		ServerName:          pluginConfiguration.SaveToServer,
	})
}
