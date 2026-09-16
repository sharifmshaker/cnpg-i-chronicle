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

// Package controller reconciles the plugin's own resources.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/machinery/pkg/log"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/operator/config"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

const (
	// resyncInterval paces what has no event to wake it: re-resolving a
	// derivedFrom ObjectStore, which is deliberately not watched, re-probing
	// the object store, and the retention sweep. Nothing depends on any of
	// them being prompt.
	resyncInterval = 10 * time.Minute

	// notReadyRetryInterval is the shorter wait while a store is not usable,
	// so fixing the cause is reflected in about a minute.
	notReadyRetryInterval = time.Minute

	// checkTimeout bounds the reachability probe. The S3 client retries with
	// backoff on its own, and an endpoint that does not answer would
	// otherwise hold a reconcile worker for minutes.
	checkTimeout = 10 * time.Second
)

// ConfigStoreReconciler keeps a ConfigStore's status honest — it publishes the
// resolved backing configuration and reports whether the store is usable — and
// enforces the store's retention on every history archived into it.
type ConfigStoreReconciler struct {
	client.Client
	Resolver store.Provider
}

// The plugin's whole RBAC footprint is declared here, for every component in
// the process, so `make manifests` renders one ClusterRole. Outside its own API
// group everything is read-only, and nothing is watched: Secrets, Clusters and
// Backups are read through uncached clients, so no informer and no watch verb.
//
// +kubebuilder:rbac:groups=chronicle.sharifmshaker.github.io,resources=configstores;restorepolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=chronicle.sharifmshaker.github.io,resources=configstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=barmancloud.cnpg.io,resources=objectstores,verbs=get
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;backups,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile resolves a ConfigStore and records what it found.
func (r *ConfigStoreReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx).WithName("configstore")

	var configStore chroniclev1.ConfigStore
	if err := r.Get(ctx, req.NamespacedName, &configStore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	original := configStore.DeepCopy()
	checked, reason, err := r.check(ctx, &configStore)
	if err != nil {
		configStore.Status.Resolved = nil
		meta.SetStatusCondition(&configStore.Status.Conditions, metav1.Condition{
			Type:    chroniclev1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: err.Error(),
		})
		contextLogger.Info("ConfigStore is not usable",
			"store", req.NamespacedName, "reason", err.Error())
		if patchErr := r.patchStatus(ctx, original, &configStore); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		// Not ready is usually a Secret or ObjectStore about to be applied, or
		// an endpoint still starting. Neither produces an event on this object,
		// so retry sooner than the resync rather than report a stale verdict
		// for ten minutes.
		return ctrl.Result{RequeueAfter: notReadyRetryInterval}, nil
	}

	message := "Object store configuration resolved from " + checked.resolved.Source
	if checked.bucketMissing {
		message += fmt.Sprintf("; bucket %q does not exist yet and is created on first write",
			checked.resolved.Destination.Bucket)
	}
	now := metav1.Now()
	configStore.Status.Resolved = &chroniclev1.ResolvedStore{
		DestinationPath: checked.resolved.Configuration.DestinationPath,
		EndpointURL:     checked.resolved.Configuration.EndpointURL,
		Provider:        checked.resolved.Provider,
		Source:          checked.resolved.Source,
		CaptureGroups:   snapshot.EffectiveGroupNames(configStore.GetCaptureGroups()),
		Retention:       describeRetention(checked.retention),
		ObservedAt:      &now,
	}
	meta.SetStatusCondition(&configStore.Status.Conditions, metav1.Condition{
		Type:    chroniclev1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  chroniclev1.ReasonResolved,
		Message: message,
	})

	r.enforceRetention(ctx, &configStore, checked)

	if err := r.patchStatus(ctx, original, &configStore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// checkedStore is everything the Ready checks established about a usable store.
type checkedStore struct {
	resolved      *store.Resolved
	backend       store.Backend
	retention     time.Duration
	bucketMissing bool
}

// check runs every check behind the Ready condition, cheapest first, and stops
// at the first failure with the reason an operator would act on.
//
// The last two are what make Ready mean "capture will work" rather than "the
// spec parses". Building the client reads the credential Secrets and parses the
// CA bundle, which catches a missing Secret or key with no network call at all.
// The probe then makes one request, so an unreachable endpoint or rejected
// credentials show up here instead of as PhaseFailurePlugin on the first
// Cluster that tries to capture.
func (r *ConfigStoreReconciler) check(
	ctx context.Context,
	configStore *chroniclev1.ConfigStore,
) (checkedStore, string, error) {
	resolved, err := r.Resolver.Resolve(ctx, configStore)
	if err != nil {
		var crdAbsent *store.ObjectStoreCRDAbsentError
		switch {
		case errors.As(err, &crdAbsent):
			return checkedStore{}, chroniclev1.ReasonObjectStoreCRDAbsent, err
		case apierrs.IsNotFound(err):
			return checkedStore{}, chroniclev1.ReasonObjectStoreMissing, err
		default:
			return checkedStore{}, chroniclev1.ReasonConfigurationInvalid, err
		}
	}

	// A capture group naming nothing is a typo, and a typo here silently
	// narrows what every cluster writing to this store captures. Catch it on
	// the store rather than letting each capture fail separately.
	if err := validateCaptureGroups(configStore.GetCaptureGroups()); err != nil {
		return checkedStore{}, chroniclev1.ReasonConfigurationInvalid, err
	}

	retention, err := effectiveRetention(configStore, resolved)
	if err != nil {
		return checkedStore{}, chroniclev1.ReasonConfigurationInvalid, err
	}

	backend, err := r.Resolver.Backend(ctx, configStore.Namespace, resolved)
	if err != nil {
		return checkedStore{}, chroniclev1.ReasonConfigurationInvalid, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	bucketMissing := false
	if err := backend.Check(probeCtx, resolved.Destination.Path); err != nil {
		if !errors.Is(err, store.ErrBucketNotFound) {
			return checkedStore{}, chroniclev1.ReasonObjectStoreUnreachable, err
		}
		bucketMissing = true
	}

	return checkedStore{
		resolved:      resolved,
		backend:       backend,
		retention:     retention,
		bucketMissing: bucketMissing,
	}, "", nil
}

// effectiveRetention resolves spec.retention to a window, where zero means
// "keep everything".
func effectiveRetention(
	configStore *chroniclev1.ConfigStore,
	resolved *store.Resolved,
) (time.Duration, error) {
	switch value := configStore.GetRetention(); value {
	case chroniclev1.RetentionForever:
		return 0, nil

	case chroniclev1.RetentionInherit:
		if resolved.InheritedRetention == "" {
			return 0, fmt.Errorf(
				"retention is %s, but the object store this derives from sets no "+
					"retentionPolicy. Set one there, or give this store an explicit "+
					"retention like 30d",
				chroniclev1.RetentionInherit)
		}
		window, err := chroniclev1.ParseRetentionWindow(resolved.InheritedRetention)
		if err != nil {
			return 0, fmt.Errorf("inherited %w", err)
		}
		return window, nil

	default:
		return chroniclev1.ParseRetentionWindow(value)
	}
}

// describeRetention renders the enforced window for status.
func describeRetention(window time.Duration) string {
	if window == 0 {
		return chroniclev1.RetentionForever
	}
	return fmt.Sprintf("%dd", int(window.Hours()/24))
}

// validateCaptureGroups rejects a selection naming a group that does not exist.
func validateCaptureGroups(configured []string) error {
	var unknown []string
	for _, name := range configured {
		if _, ok := snapshot.LookupGroup(name); !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown capture group(s) %s; valid groups are %s",
		strings.Join(unknown, ", "), strings.Join(snapshot.GroupNames(), ", "))
}

// enforceRetention removes snapshots the retention guarantee has superseded,
// for every Cluster in this namespace that archives into this store.
//
// The set of histories comes from the Clusters rather than from the bucket,
// because Backend.List is flat: enumerating server names would mean listing the
// store root, which also holds barman's base backups and every WAL segment —
// orders of magnitude more keys than there are clusters, for the same answer.
// S3 could answer it cheaply with a delimiter listing, but that is not part of
// the Backend interface, and adding it would only be worth doing for a feature
// that wanted it.
//
// It follows that a history outlives the Cluster that wrote it: a deleted
// cluster, or one that repoints saveToStore, leaves snapshots nothing prunes.
//
// That matches barman-cloud, which is the more useful thing to know. Its
// retention runs in the instance sidecar on the current primary
// (CatalogMaintenanceRunnable, every five minutes by default) and calls
// DeleteBackupsByPolicy for that pod's own serverName. Delete the Cluster and
// the sidecar goes with it, so the base backups and WAL are equally left
// behind. Neither plugin has a reaper that walks the bucket.
//
// Keeping the two consistent matters more than reclaiming the space. Pruning
// orphaned configuration while barman keeps the data would leave backups that
// can still be restored and no record of the configuration that ran alongside
// them, which inverts the point of writing both to one bucket.
func (r *ConfigStoreReconciler) enforceRetention(
	ctx context.Context,
	configStore *chroniclev1.ConfigStore,
	checked checkedStore,
) {
	if checked.retention == 0 || checked.bucketMissing {
		return
	}

	contextLogger := log.FromContext(ctx).WithName("configstore")

	var clusters apiv1.ClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(configStore.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return // CloudNativePG is not installed.
		}
		contextLogger.Info("cannot list clusters for retention",
			"store", configStore.Name, "reason", err.Error())
		return
	}

	cutoff := time.Now().UTC().Add(-checked.retention)
	for i := range clusters.Items {
		configuration := config.NewFromCluster(&clusters.Items[i])
		if !configuration.IsSaveEnabled() || configuration.SaveToStore != configStore.Name {
			continue
		}
		r.pruneSuperseded(ctx, checked.backend, configStore, checked.resolved, configuration.SaveToServer, cutoff)
	}
}

// pruneSuperseded applies the retention guarantee to one history.
//
// Failures are logged and not propagated. Retention is housekeeping: a store
// that cannot delete is still capturing and still restorable, and turning a
// missing s3:DeleteObject grant into a NotReady store would take a working
// setup offline over disk usage.
func (r *ConfigStoreReconciler) pruneSuperseded(
	ctx context.Context,
	backend store.Backend,
	configStore *chroniclev1.ConfigStore,
	resolved *store.Resolved,
	serverName string,
	cutoff time.Time,
) {
	contextLogger := log.FromContext(ctx).WithName("configstore")

	snapshots := &store.SnapshotStore{
		Backend: backend,
		Layout:  store.LayoutFor(configStore, resolved, serverName),
	}
	refs, err := snapshots.List(ctx)
	if err != nil {
		contextLogger.Info("cannot list snapshots for retention",
			"store", configStore.Name, "server", serverName, "reason", err.Error())
		return
	}

	for _, key := range store.PlanRetention(refs, cutoff) {
		if err := backend.Delete(ctx, key); err != nil {
			contextLogger.Info("cannot delete a superseded snapshot",
				"store", configStore.Name, "server", serverName,
				"key", key, "reason", err.Error())
			return
		}
		contextLogger.Info("deleted a snapshot superseded by the retention anchor",
			"store", configStore.Name, "server", serverName, "key", key)
	}
}

// patchStatus writes the resolution and conditions when they changed.
//
// A merge patch with no optimistic lock: this controller is the only writer of
// ConfigStore status, and what it writes is a function of the spec and the
// referenced objects, so two replicas racing here write the same bytes.
func (r *ConfigStoreReconciler) patchStatus(
	ctx context.Context,
	original *chroniclev1.ConfigStore,
	updated *chroniclev1.ConfigStore,
) error {
	if equalStatus(original, updated) {
		return nil
	}
	return r.Status().Patch(ctx, updated, client.MergeFrom(original))
}

// SetupWithManager wires the controller.
//
// It deliberately does not watch barmancloud.cnpg.io ObjectStore resources.
// controller-runtime cannot start an informer for a CRD that is not installed
// and fails the whole manager when it tries, which would make the plugin
// unusable on any cluster without the barman-cloud plugin — precisely the
// deployment the standalone spec.configuration path exists to serve. Derived
// configurations are instead re-resolved on the resync interval and through the
// short-lived cache in store.ObjectStoreReader.
//
// The generation predicate keeps this controller's own status patch from
// waking it again; the resync covers everything a spec change does not.
func (r *ConfigStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&chroniclev1.ConfigStore{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("configstore").
		Complete(r)
}
