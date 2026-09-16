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

// Package webhook implements the admission webhooks the plugin serves.
//
// Restore runs here rather than through CNPG-I. The protocol defines
// Operator.MutateCluster for exactly this purpose, but CloudNativePG has not
// called it since remote plugin support replaced the unix-socket loader its
// admission code used to build, and the upstream request to reinstate it was
// closed as not planned. Doing this at CREATE is not merely a workaround
// though: it is what makes a faithful restore possible at all, because the
// update path forbids shrinking storage and changing postgresUID, and rejects
// an image and GUC change in the same request.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/log"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/operator/config"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/restore"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// resolveTimeout bounds everything the handler does.
//
// This runs inside admission, so the API server is holding a client's Cluster
// creation open while we talk to an object store. The webhook is registered
// with timeoutSeconds 10; finishing meaningfully sooner means a slow bucket
// produces our explanatory message rather than the API server's generic
// timeout.
const resolveTimeout = 8 * time.Second

// ClusterRestorer injects a captured configuration into a Cluster at creation.
type ClusterRestorer struct {
	// Client must read straight from the API server, not from an informer
	// cache.
	//
	// A RestorePolicy and the Cluster that uses it are routinely applied
	// together — they are two documents in one manifest, and kubectl creates
	// them milliseconds apart. A cached read loses that race and rejects the
	// Cluster with "RestorePolicy does not exist", which is both wrong and
	// intermittent. Admission decisions need read-after-write, so this is wired
	// to the manager's uncached API reader.
	//
	// It is a Reader rather than a Client because this webhook only ever reads;
	// the only thing it writes is the patch in its own admission response.
	Client   client.Reader
	Resolver store.Provider
}

// RestoreRecord is written to the restored-from annotation. Its absence is what
// the Pre-reconcile guard treats as proof this webhook never ran.
type RestoreRecord struct {
	Store       string    `json:"store"`
	Policy      string    `json:"policy"`
	ServerName  string    `json:"serverName"`
	SnapshotKey string    `json:"snapshotKey"`
	Generation  int64     `json:"generation"`
	CapturedAt  time.Time `json:"capturedAt"`
	RestoredAt  time.Time `json:"restoredAt"`
	Changed     int       `json:"changedPaths"`
	Skipped     int       `json:"skippedPaths"`

	// AlignedWith explains how the snapshot was chosen when the policy aligned
	// with the cluster's recovery target.
	AlignedWith string `json:"alignedWith,omitempty"`

	// Transformed lists the paths an expression rewrote on the way in, so a
	// restored value that does not match the source is traceable.
	Transformed []string `json:"transformed,omitempty"`
}

// Handle mutates a Cluster being created.
func (r *ClusterRestorer) Handle(ctx context.Context, request admission.Request) admission.Response {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	contextLogger := log.FromContext(ctx).WithName("restore_webhook")

	cluster, err := pathutil.FromJSON(request.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest,
			fmt.Errorf("cannot decode the Cluster being created: %w", err))
	}

	namespace := request.Namespace
	if namespace == "" {
		if value, found := pathutil.Get(cluster, "metadata.namespace"); found {
			namespace, _ = value.(string)
		}
	}
	name, _ := pathutil.Get(cluster, "metadata.name")

	parameters, present := pluginParameters(cluster)
	if !present {
		// matchConditions should keep us from being called at all in this case;
		// allowing rather than erroring keeps a stale webhook registration from
		// blocking unrelated clusters.
		return admission.Allowed("the chronicle plugin is not configured on this cluster")
	}

	clusterName, _ := name.(string)
	configuration := config.NewFromParameters(parameters, clusterName)
	configuration.Present, configuration.Enabled = true, true

	// A half-specified restore is refused rather than guessed at: a store with
	// no server says where to look but not for what.
	if err := configuration.Validate(); err != nil {
		return admission.Denied(err.Error())
	}
	if err := checkSourceIsNotAlsoDestination(configuration); err != nil {
		return admission.Denied(err.Error())
	}
	if !configuration.IsRestoreRequested() {
		return admission.Allowed("no chronicle restore configured")
	}

	// Once the annotation exists the restore has already happened. Admission
	// can be re-invoked (reinvocationPolicy, or a client retry), and a second
	// pass must not re-resolve Latest and land a different snapshot.
	if _, found := pathutil.GetSegments(cluster,
		[]string{"metadata", "annotations", metadata.AnnotationRestoredFrom}); found {
		return admission.Allowed("already restored")
	}

	record, patched, err := r.restore(ctx, namespace, configuration, cluster)
	if err != nil {
		contextLogger.Error(err, "refusing cluster creation",
			"cluster", name, "namespace", namespace,
			"store", configuration.RestoreFromStore,
			"server", configuration.RestoreFromServer,
			"policy", configuration.RestorePolicy)
		// Denied, not Errored: failurePolicy is Fail, and a clear message on the
		// kubectl apply is far more useful than a generic webhook failure.
		return admission.Denied(err.Error())
	}

	contextLogger.Info("restored cluster configuration",
		"cluster", name, "namespace", namespace,
		"policy", configuration.RestorePolicy, "snapshot", record.SnapshotKey,
		"changed", record.Changed, "skipped", record.Skipped)

	return admission.PatchResponseFromRaw(request.Object.Raw, patched)
}

func (r *ClusterRestorer) restore(
	ctx context.Context,
	namespace string,
	configuration *config.PluginConfiguration,
	cluster map[string]any,
) (*RestoreRecord, []byte, error) {
	// The source comes from the Cluster, the rules from an optional policy.
	// Keeping them apart is what lets one policy serve every restore: "a tenth
	// of production's size" is a statement about an environment, not about
	// which history it reads.
	configStore, policy, err := r.load(ctx, namespace, configuration)
	if err != nil {
		return nil, nil, err
	}

	// Rules are compiled before the store is touched: a policy that could never
	// work is refused for the reason the policy's own status would give, and
	// with no object-store round trip spent finding that out.
	skips, transforms, err := restore.CompileRules(policySkips(policy), policyTransforms(policy))
	if err != nil {
		return nil, nil, fmt.Errorf("RestorePolicy %q is not usable: %w", configuration.RestorePolicy, err)
	}

	source, err := store.Open(ctx, r.Resolver, configStore, configuration.RestoreFromServer)
	if err != nil {
		return nil, nil, fmt.Errorf("while opening ConfigStore %q: %w", configStore.Name, err)
	}

	_, bootstrapsFromRecovery := pathutil.Get(cluster, "spec.bootstrap.recovery")
	mode := chroniclev1.ResolveMode(policy, bootstrapsFromRecovery)

	resolution, err := restore.Select(ctx, source, mode, policy, &restore.Alignment{
		Cluster:   cluster,
		Namespace: namespace,
		Backups:   restore.KubeBackupLookup{Client: r.Client},
	})
	if err != nil {
		return nil, nil, fmt.Errorf(
			"while selecting a snapshot for server %q in ConfigStore %q: %w",
			configuration.RestoreFromServer, configStore.Name, err)
	}

	plan, err := restore.BuildPlan(resolution.Snapshot, skips, transforms)
	if err != nil {
		return nil, nil, err
	}
	// A restore that changes nothing is a misconfiguration, not a success. It
	// means every captured path was skipped or denied, or the snapshot holds
	// nothing restorable — and annotating the cluster as restored would leave
	// an operator believing configuration was applied when none was.
	if len(plan.Changes) == 0 {
		return nil, nil, fmt.Errorf(
			"restoring from %s would change nothing: the snapshot has %d paths and all of "+
				"them are skipped or denied. Check the skip rules on RestorePolicy %q",
			resolution.Key, len(plan.Exclusions), configuration.RestorePolicy)
	}

	if err := plan.Apply(cluster); err != nil {
		return nil, nil, err
	}

	record := &RestoreRecord{
		Store:       configStore.Name,
		Policy:      configuration.RestorePolicy,
		ServerName:  configuration.RestoreFromServer,
		SnapshotKey: resolution.Key,
		Generation:  resolution.Snapshot.Generation,
		CapturedAt:  resolution.Snapshot.CapturedAt,
		RestoredAt:  time.Now().UTC(),
		Changed:     len(plan.Changes),
		Skipped:     len(plan.Exclusions),
		Transformed: plan.TransformedPaths(),
	}
	if resolution.Target != nil {
		record.AlignedWith = resolution.Target.Detail
	}

	patched, err := annotateWithRecord(cluster, record)
	if err != nil {
		return nil, nil, err
	}
	return record, patched, nil
}

// load reads the ConfigStore the Cluster restores from, and the RestorePolicy
// it names if it names one.
//
// The not-found messages are deliberately specific. This runs inside admission,
// so the message is the only thing the operator sees: naming which object is
// missing turns a rejected `kubectl apply` into an obvious fix.
func (r *ClusterRestorer) load(
	ctx context.Context,
	namespace string,
	configuration *config.PluginConfiguration,
) (*chroniclev1.ConfigStore, *chroniclev1.RestorePolicy, error) {
	var configStore chroniclev1.ConfigStore
	storeKey := types.NamespacedName{Namespace: namespace, Name: configuration.RestoreFromStore}
	if err := r.Client.Get(ctx, storeKey, &configStore); err != nil {
		if apierrs.IsNotFound(err) {
			return nil, nil, fmt.Errorf(
				"%s names ConfigStore %q, which does not exist in namespace %q",
				metadata.ParameterRestoreFromStore, configuration.RestoreFromStore, namespace)
		}
		return nil, nil, fmt.Errorf("while reading ConfigStore %s: %w", storeKey, err)
	}

	// No policy named means "restore this history as it was", which is a
	// complete instruction on its own.
	if configuration.RestorePolicy == "" {
		return &configStore, nil, nil
	}

	var policy chroniclev1.RestorePolicy
	policyKey := types.NamespacedName{Namespace: namespace, Name: configuration.RestorePolicy}
	if err := r.Client.Get(ctx, policyKey, &policy); err != nil {
		if apierrs.IsNotFound(err) {
			return nil, nil, fmt.Errorf(
				"RestorePolicy %q does not exist in namespace %q; create it before "+
					"creating a cluster that restores with it",
				configuration.RestorePolicy, namespace)
		}
		return nil, nil, fmt.Errorf("while reading RestorePolicy %s: %w", policyKey, err)
	}

	return &configStore, &policy, nil
}

// policySkips and policyTransforms read rules from a policy that may not exist.
// No policy means no rules, which BuildPlan already handles as "restore
// everything the snapshot holds".
func policySkips(policy *chroniclev1.RestorePolicy) []chroniclev1.SkipRule {
	if policy == nil {
		return nil
	}
	return policy.Spec.Skip
}

func policyTransforms(policy *chroniclev1.RestorePolicy) []chroniclev1.TransformRule {
	if policy == nil {
		return nil
	}
	return policy.Spec.Transform
}

// annotateWithRecord stamps the restore record onto the cluster and returns the
// re-encoded object for the admission response.
func annotateWithRecord(cluster map[string]any, record *RestoreRecord) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("while recording the restore: %w", err)
	}

	// Segments, not a dotted path: the annotation key contains dots, and
	// splitting it would create {"chronicle": {"sharifmshaker": ...}} instead of
	// one annotation — leaving the Pre-reconcile guard convinced the restore
	// never ran.
	if !pathutil.SetSegments(cluster,
		[]string{"metadata", "annotations", metadata.AnnotationRestoredFrom}, string(encoded)) {
		return nil, fmt.Errorf("cannot annotate the cluster with the restore record")
	}

	patched, err := json.Marshal(cluster)
	if err != nil {
		return nil, fmt.Errorf("while re-encoding the restored cluster: %w", err)
	}
	return patched, nil
}

// checkSourceIsNotAlsoDestination refuses a cluster that would archive into the
// same history it restores from. Both sides are plugin parameters on this one
// Cluster, so it is a straight comparison of what the manifest says.
func checkSourceIsNotAlsoDestination(configuration *config.PluginConfiguration) error {
	if configuration.SaveToStore == "" || configuration.RestoreFromStore == "" {
		return nil
	}
	if configuration.SaveToStore != configuration.RestoreFromStore {
		return nil
	}
	if configuration.SaveToServer != configuration.RestoreFromServer {
		return nil
	}

	return fmt.Errorf(
		"this cluster would save snapshots to the same store %q and server name %q it is "+
			"restoring from, which would interleave its history with the source cluster's. "+
			"Point %s at a different store, or set %s to a distinct server name",
		configuration.SaveToStore, configuration.SaveToServer,
		metadata.ParameterSaveToStore, metadata.ParameterSaveToServer)
}

// pluginParameters reads this plugin's parameters off a Cluster in its generic
// JSON form. The second result is false when the plugin is not listed, or is
// listed but disabled.
func pluginParameters(cluster map[string]any) (map[string]string, bool) {
	raw, found := pathutil.Get(cluster, "spec.plugins")
	if !found {
		return nil, false
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, false
	}

	for _, entry := range entries {
		declaration, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := declaration["name"].(string); name != metadata.PluginName {
			continue
		}
		if enabled, present := declaration["enabled"].(bool); present && !enabled {
			return nil, false
		}

		parameters, ok := declaration["parameters"].(map[string]any)
		if !ok {
			return map[string]string{}, true
		}
		out := make(map[string]string, len(parameters))
		for key, value := range parameters {
			if asString, ok := value.(string); ok {
				out[key] = asString
			}
		}
		return out, true
	}
	return nil, false
}
