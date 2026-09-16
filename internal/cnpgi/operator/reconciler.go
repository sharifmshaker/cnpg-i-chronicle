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
	"errors"
	"fmt"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/decoder"
	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/object"
	"github.com/cloudnative-pg/cnpg-i/pkg/reconciler"
	"github.com/cloudnative-pg/machinery/pkg/log"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/operator/config"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// ReconcilerImplementation serves the ReconcilerHooks service.
type ReconcilerImplementation struct {
	reconciler.UnimplementedReconcilerHooksServer

	Client   client.Client
	Resolver store.Provider

	// Settled skips the object-store round trip for cluster states already
	// known to need no capture. Optional: a nil cache simply never hits.
	Settled *settledCache
}

// GetCapabilities declares that the plugin hooks the Cluster reconciler.
func (r ReconcilerImplementation) GetCapabilities(
	_ context.Context,
	_ *reconciler.ReconcilerHooksCapabilitiesRequest,
) (*reconciler.ReconcilerHooksCapabilitiesResult, error) {
	return &reconciler.ReconcilerHooksCapabilitiesResult{
		ReconcilerCapabilities: []*reconciler.ReconcilerHooksCapability{
			{Kind: reconciler.ReconcilerHooksCapability_KIND_CLUSTER},
		},
	}, nil
}

var (
	resultContinue = &reconciler.ReconcilerHooksResult{
		Behavior: reconciler.ReconcilerHooksResult_BEHAVIOR_CONTINUE,
	}
	resultRequeue = &reconciler.ReconcilerHooksResult{
		Behavior: reconciler.ReconcilerHooksResult_BEHAVIOR_REQUEUE,
	}
)

// Pre guards against a cluster whose restore silently never happened.
//
// Restore is applied by this plugin's mutating admission webhook at CREATE. If
// the webhook did not run — the plugin was installed after the cluster, or its
// MutatingWebhookConfiguration was removed — the cluster would come up with the
// operator's defaults instead of the configuration it was supposed to inherit,
// and nothing would say so. Refusing here turns that into a visible
// PhaseFailurePlugin rather than a database quietly running on the wrong
// settings.
//
// This is a detector, not a second restore path. Restoring here instead would
// mean a weaker implementation constrained by CNPG's update-immutability rules
// (storage could only grow, UID/GID could not be set), and two code paths whose
// results differ is worse than one that fails loudly.
func (r ReconcilerImplementation) Pre(
	ctx context.Context,
	request *reconciler.ReconcilerHooksRequest,
) (*reconciler.ReconcilerHooksResult, error) {
	contextLogger := log.FromContext(ctx).WithName("chronicle_pre")

	kind, err := object.GetKind(request.GetResourceDefinition())
	if err != nil {
		return nil, err
	}
	if kind != "Cluster" {
		return resultContinue, nil
	}

	cluster, err := decoder.DecodeClusterLenient(request.GetClusterDefinition())
	if err != nil {
		return nil, err
	}

	pluginConfiguration := config.NewFromCluster(cluster)
	if !pluginConfiguration.IsRestoreRequested() {
		return resultContinue, nil
	}
	if _, applied := cluster.Annotations[metadata.AnnotationRestoredFrom]; applied {
		return resultContinue, nil
	}

	contextLogger.Error(nil, "restore was configured but never applied",
		"cluster", cluster.Name,
		"restoreFromStore", pluginConfiguration.RestoreFromStore,
		"restoreFromServer", pluginConfiguration.RestoreFromServer)

	return nil, fmt.Errorf(
		"cluster %q sets the %s plugin parameter %q=%q but carries no %s annotation, "+
			"which means the chronicle mutating webhook did not run for it. The cluster "+
			"has NOT inherited the configuration it was meant to restore. Verify the "+
			"MutatingWebhookConfiguration %q exists and the plugin was reachable when the "+
			"cluster was created, then recreate the cluster",
		cluster.Name, metadata.PluginName, metadata.ParameterRestoreFromStore,
		pluginConfiguration.RestoreFromStore, metadata.AnnotationRestoredFrom,
		metadata.MutatingWebhookName)
}

// liveFingerprint is the fingerprint of a Cluster as it stands, to compare with
// the one recomputed from the newest snapshot.
func liveFingerprint(cluster *apiv1.Cluster, configuredGroups []string) (string, error) {
	groups := snapshot.EffectiveGroupNames(configuredGroups)
	return snapshot.MetadataFingerprint(
		cluster.Labels, cluster.Annotations, groups, snapshot.CaptureRulesDigest(groups))
}

// Post captures a snapshot when the Cluster's generation has moved.
//
// Post rather than Pre because it runs from finalizeReconciliation, at the end
// of a successful loop, when the spec has settled and the operator has applied
// its own defaults.
func (r ReconcilerImplementation) Post(
	ctx context.Context,
	request *reconciler.ReconcilerHooksRequest,
) (*reconciler.ReconcilerHooksResult, error) {
	contextLogger := log.FromContext(ctx).WithName("chronicle_post")

	kind, err := object.GetKind(request.GetResourceDefinition())
	if err != nil {
		return nil, err
	}
	if kind != "Cluster" {
		return resultContinue, nil
	}

	cluster, err := decoder.DecodeClusterLenient(request.GetClusterDefinition())
	if err != nil {
		return nil, err
	}

	pluginConfiguration := config.NewFromCluster(cluster)
	if !pluginConfiguration.IsSaveEnabled() {
		return resultContinue, nil
	}

	storeKey := pluginConfiguration.SaveToKey()
	var configStore chroniclev1.ConfigStore
	if err := r.Client.Get(ctx, storeKey, &configStore); err != nil {
		if apierrs.IsNotFound(err) {
			// A dependency that has not appeared yet is a requeue, not an error;
			// an error would drive the Cluster into PhaseFailurePlugin for what
			// is usually just apply ordering.
			contextLogger.Info("waiting for ConfigStore to exist", "store", storeKey)
			return resultRequeue, nil
		}
		return nil, err
	}

	// Generation alone misses label and annotation edits, which do not bump
	// it, and changes to the capture rules, which do not touch the Cluster at
	// all. The fingerprint covers both and is cheap enough to compute on every
	// reconcile, unlike a full capture.
	captureGroups := configStore.GetCaptureGroups()
	metadataFingerprint, err := liveFingerprint(cluster, captureGroups)
	if err != nil {
		return nil, fmt.Errorf("while fingerprinting metadata of cluster %q: %w", cluster.Name, err)
	}

	// Fast path. Reconciliation runs constantly and almost every pass finds a
	// state already captured, so that case must cost nothing.
	//
	// Two things can answer it without any I/O. The watermark arrives inside the
	// Cluster this hook was handed, so it needs no read at all. The cache covers
	// what the watermark structurally cannot: a generation bump that changed
	// nothing captured leaves the newest snapshot — and therefore the watermark —
	// on the older generation forever.
	watermark, hasWatermark := WatermarkFromCluster(cluster)
	if hasWatermark &&
		watermark.LastSavedGeneration == cluster.Generation &&
		watermark.MetadataChecksum == metadataFingerprint {
		return resultContinue, nil
	}
	if _, ok := r.Settled.settled(cluster, metadataFingerprint); ok {
		return resultContinue, nil
	}

	snap, err := snapshot.Extract(cluster, snapshot.ExtractOptions{
		Groups:        captureGroups,
		PluginVersion: metadata.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("while capturing cluster %q: %w", cluster.Name, err)
	}

	snapshotStore, err := store.Open(ctx, r.Resolver, &configStore, pluginConfiguration.SaveToServer)
	if err != nil {
		return nil, fmt.Errorf("while reaching ConfigStore %s: %w", storeKey, err)
	}

	// The watermark is published by CloudNativePG on its own schedule, so
	// between a capture and the next status refresh it still shows the previous
	// one. Rather than trusting that lag, confirm against the store itself: if
	// the newest object already holds this exact content, the capture happened
	// and only the status has yet to catch up.
	//
	// This also covers a generation bump that changed nothing we capture — a
	// field outside every group, or one the denylist strips — which produces an
	// identical checksum and needs no second object.
	//
	// The rules have to match as well. A snapshot with the same content but
	// captured under older rules is superseded, so that a plugin upgrade which
	// captures more — or differently — leaves a snapshot saying so, and the
	// watermark lines up with the new rules instead of missing forever.
	if latest, latestKey, err := snapshotStore.Latest(ctx); err == nil {
		if latest.Checksum == snap.Checksum && latest.CaptureRules == snap.CaptureRules {
			contextLogger.Debug("nothing new to capture",
				"cluster", cluster.Name, "generation", cluster.Generation)
			// Remembered so the next call of either hook for this same state
			// costs nothing.
			r.Settled.remember(cluster, metadataFingerprint, latestKey)
			return resultContinue, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("while reading the latest snapshot for cluster %q: %w",
			cluster.Name, err)
	}

	key, err := snapshotStore.Write(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("while writing snapshot for cluster %q: %w", cluster.Name, err)
	}

	contextLogger.Info("captured cluster configuration",
		"cluster", cluster.Name,
		"generation", cluster.Generation,
		"key", key,
		"store", storeKey)

	// The status watermark will say the same thing once CloudNativePG refreshes
	// it; until then this covers the gap.
	r.Settled.remember(cluster, metadataFingerprint, key)

	return resultContinue, nil
}
