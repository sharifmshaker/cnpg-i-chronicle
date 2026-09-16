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

package v1

import (
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultPrefix is the key prefix, relative to <destinationPath>/<serverName>/,
	// under which snapshots are written. It deliberately sits alongside barman-cloud's
	// "base/" and "wals/" directories rather than inside either of them: barman only
	// ever enumerates those two prefixes and deletes by explicit key, so a sibling
	// prefix cannot be disturbed by backup retention.
	DefaultPrefix = "chronicle"

	// ConditionReady reports whether an object is usable. For a ConfigStore,
	// its configuration resolves, its retention and capture groups are valid,
	// its credential Secrets can be read, and the object store accepts a
	// listing with them. For a RestorePolicy, its rules are coherent.
	ConditionReady = "Ready"
)

// Condition reasons reported on a ConfigStore and a RestorePolicy.
const (
	ReasonResolved               = "Resolved"
	ReasonObjectStoreMissing     = "ObjectStoreNotFound"
	ReasonObjectStoreCRDAbsent   = "ObjectStoreCRDNotInstalled"
	ReasonConfigurationInvalid   = "ConfigurationInvalid"
	ReasonObjectStoreUnreachable = "ObjectStoreUnreachable"
)

// BarmanObjectStoreRef points at a barman-cloud ObjectStore in the same namespace,
// from which this store's backing configuration is derived.
type BarmanObjectStoreRef struct {
	// Name of a barmancloud.cnpg.io/v1 ObjectStore in the same namespace as this
	// ConfigStore. Only its .spec.configuration is consumed; barman-owned fields
	// such as retentionPolicy and instanceSidecarConfiguration are ignored.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ConfigStoreSpec defines where cluster configuration snapshots are stored.
//
// Exactly one of derivedFrom or configuration must be set. derivedFrom lets the
// store piggyback on an object store that is already configured for backups;
// configuration lets it stand alone, so the plugin is usable without the
// barman-cloud plugin installed.
// +kubebuilder:validation:XValidation:rule="has(self.derivedFrom) != has(self.configuration)",message="exactly one of derivedFrom or configuration must be set"
type ConfigStoreSpec struct {
	// DerivedFrom resolves the backing configuration from an existing barman-cloud
	// ObjectStore rather than restating it here.
	// +optional
	DerivedFrom *BarmanObjectStoreRef `json:"derivedFrom,omitempty"`

	// Configuration is a self-contained object store definition, scoped to what
	// this plugin actually uses. Credential field names match barman-cloud's, so
	// that block can be copied across from a backup configuration unchanged.
	//
	// There is no serverName here: one store serves many clusters, and the
	// per-cluster server name comes from the plugin's serverName parameter.
	// +optional
	Configuration *ObjectStoreConfiguration `json:"configuration,omitempty"`

	// Retention is how far back a restore must still be able to reach.
	//
	// This is a guarantee, not a deletion window. "30d" means "any restore
	// target within the last 30 days must resolve", and honouring it can mean
	// keeping a snapshot that is a year old: snapshots are written only when
	// configuration changes, so the newest snapshot at or before the window's
	// start may be very old, and it is the answer to every query back to that
	// point. Deleting it would leave those restores with nothing to select.
	//
	// Only superseded history is removed — snapshots older than that anchor,
	// which no target inside the window could ever select. A cluster that has
	// not changed in a year keeps its single snapshot forever.
	//
	// Values:
	//
	//   Forever                  never delete anything. The default.
	//   30d, 4w, 6m              an explicit window
	//   InheritFromObjectStore   follow retentionPolicy on the barman
	//                            ObjectStore this store derives from
	//
	// Targets older than the window still resolve, but to the oldest retained
	// configuration, which may be later than the one actually in force then. In
	// practice that is moot: backup retention means there is no data to pair an
	// older configuration with.
	//
	// Enforcement needs s3:DeleteObject. Without it, deletions are logged and
	// skipped: the store keeps capturing and restoring rather than going
	// NotReady over disk usage.
	// +kubebuilder:validation:Pattern=`^(Forever|InheritFromObjectStore|[1-9][0-9]*[dwm])$`
	// +optional
	Retention string `json:"retention,omitempty"`

	// CaptureGroups selects which capture groups are written into snapshots
	// stored here. Empty means the default set.
	//
	// Two groups are defined but off by default, because they are correct to
	// restore in some topologies and actively harmful in others:
	//
	//   Identity  postgresUID, postgresGID, serviceAccountName
	//   Secrets   certificates, superuserSecret, enableSuperuserAccess
	//
	// Naming any group here replaces the default set rather than adding to it,
	// so list every group you want. `kubectl explain` cannot enumerate them;
	// an unknown name is rejected in status with the valid list.
	//
	// This is per-store rather than per-cluster on purpose: every snapshot in
	// one store then has the same shape, so a restore reading them does not
	// have to reason about which fields a given snapshot could contain.
	//
	// Changing this re-captures every cluster writing here, once: the effective
	// groups and the rules they capture under are part of the metadata
	// fingerprint, so the watermarks that would otherwise short-circuit capture
	// no longer match.
	// +optional
	CaptureGroups []string `json:"captureGroups,omitempty"`

	// Prefix is the key prefix, relative to <destinationPath>/<serverName>/, under
	// which snapshots are written.
	// +kubebuilder:default:=chronicle
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	// +kubebuilder:validation:XValidation:rule="self != 'base' && self != 'wals'",message="prefix must not collide with barman-cloud's 'base' or 'wals' directories"
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// ResolvedStore reports the backing configuration the plugin actually used, so that
// a derivedFrom indirection is observable without reading the source ObjectStore.
type ResolvedStore struct {
	// DestinationPath is the object store root, e.g. s3://backups/
	// +optional
	DestinationPath string `json:"destinationPath,omitempty"`

	// EndpointURL is the S3-compatible endpoint, when one is configured.
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`

	// Provider is the detected credential provider: s3, azure or google.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Retention is the effective retention, with Forever and
	// InheritFromObjectStore already resolved to what is enforced. Reported
	// because neither is legible from the spec alone.
	// +optional
	Retention string `json:"retention,omitempty"`

	// CaptureGroups is the group set snapshots here are actually written with,
	// with "empty means the default set" already resolved. Reported because the
	// default is otherwise invisible: an empty spec.captureGroups says nothing
	// about what is being captured.
	// +optional
	CaptureGroups []string `json:"captureGroups,omitempty"`

	// Source records where the configuration came from: "Inline", or
	// "ObjectStore/<name>" when derived.
	// +optional
	Source string `json:"source,omitempty"`

	// ObservedAt is when the configuration was last resolved.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ConfigStoreStatus defines the observed state of ConfigStore.
type ConfigStoreStatus struct {
	// Resolved is the backing configuration currently in effect.
	// +optional
	Resolved *ResolvedStore `json:"resolved,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=mdstore;mdstores
// +kubebuilder:printcolumn:name="Destination",type=string,JSONPath=`.status.resolved.destinationPath`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.resolved.source`
// +kubebuilder:printcolumn:name="Retention",type=string,JSONPath=`.status.resolved.retention`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConfigStore is where a cluster's configuration snapshots are kept. It can
// derive its backing object store from a barman-cloud ObjectStore, so snapshots
// land in the same bucket as backups, or define one itself.
type ConfigStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ConfigStoreSpec `json:"spec,omitempty"`
	// +optional
	Status ConfigStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConfigStoreList contains a list of ConfigStore.
type ConfigStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigStore `json:"items"`
}

// Retention modes.
const (
	// RetentionForever keeps every snapshot. The default when unset.
	RetentionForever = "Forever"

	// RetentionInherit follows the derived barman ObjectStore's retentionPolicy.
	RetentionInherit = "InheritFromObjectStore"
)

// GetRetention returns the configured retention, defaulting to Forever.
func (s *ConfigStore) GetRetention() string {
	if s.Spec.Retention == "" {
		return RetentionForever
	}
	return s.Spec.Retention
}

// ParseRetentionWindow turns a "30d" / "4w" / "6m" value into a duration.
//
// The grammar matches barman-cloud's retentionPolicy so a value can be copied
// between the two. A month is 30 days, as it is there — this bounds how much
// history to keep, and does not need calendar precision.
func ParseRetentionWindow(value string) (time.Duration, error) {
	if len(value) < 2 {
		return 0, fmt.Errorf("retention %q is not a duration like 30d, 4w or 6m", value)
	}

	count, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || count < 1 {
		return 0, fmt.Errorf("retention %q is not a duration like 30d, 4w or 6m", value)
	}

	day := 24 * time.Hour
	switch value[len(value)-1] {
	case 'd':
		return time.Duration(count) * day, nil
	case 'w':
		return time.Duration(count) * 7 * day, nil
	case 'm':
		return time.Duration(count) * 30 * day, nil
	default:
		return 0, fmt.Errorf("retention %q has an unknown unit; use d, w or m", value)
	}
}

// GetCaptureGroups returns the configured capture groups, or nil for the
// default set. Resolving "nil means default" is left to the snapshot package,
// which owns the group definitions.
func (s *ConfigStore) GetCaptureGroups() []string {
	return s.Spec.CaptureGroups
}

// GetPrefix returns the configured key prefix, falling back to the default when
// the CRD default has not been applied (e.g. objects built in tests).
func (s *ConfigStore) GetPrefix() string {
	if s.Spec.Prefix == "" {
		return DefaultPrefix
	}
	return s.Spec.Prefix
}
