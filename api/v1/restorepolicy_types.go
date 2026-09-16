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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SelectMode chooses which snapshot a restore uses.
// +kubebuilder:validation:Enum=Latest;Generation;Timestamp;AlignWithRecoveryTarget
type SelectMode string

const (
	// SelectLatest uses the most recent snapshot.
	SelectLatest SelectMode = "Latest"

	// SelectGeneration uses the snapshot captured at an exact Cluster generation.
	SelectGeneration SelectMode = "Generation"

	// SelectTimestamp uses the newest snapshot captured at or before a time.
	SelectTimestamp SelectMode = "Timestamp"

	// SelectAlignWithRecoveryTarget derives the instant from the Cluster's own
	// bootstrap.recovery stanza and uses the configuration that was in force
	// then.
	//
	// Restoring a database to last Tuesday and then applying today's
	// configuration produces a cluster that never existed. This mode keeps the
	// two in step without the operator having to copy the timestamp by hand.
	SelectAlignWithRecoveryTarget SelectMode = "AlignWithRecoveryTarget"
)

// UnresolvableAction is what to do when a recovery target cannot be mapped to a
// moment.
// +kubebuilder:validation:Enum=Fail;UseLatest
type UnresolvableAction string

const (
	// UnresolvableFail refuses to create the cluster.
	UnresolvableFail UnresolvableAction = "Fail"

	// UnresolvableUseLatest falls back to the most recent configuration.
	UnresolvableUseLatest UnresolvableAction = "UseLatest"
)

// MissingAction is what to do when a transform's path is absent from the
// snapshot being restored.
// +kubebuilder:validation:Enum=Fail;Ignore
type MissingAction string

const (
	// MissingFail refuses to create the cluster.
	MissingFail MissingAction = "Fail"

	// MissingIgnore restores everything else and leaves the path alone.
	MissingIgnore MissingAction = "Ignore"
)

// RestoreSelect picks one snapshot out of the source's history.
// +kubebuilder:validation:XValidation:rule="self.mode != 'Generation' || has(self.generation)",message="select.generation is required when mode is Generation"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Timestamp' || has(self.timestamp)",message="select.timestamp is required when mode is Timestamp"
type RestoreSelect struct {
	// Mode is how the snapshot is chosen.
	// +kubebuilder:default:=Latest
	// +optional
	Mode SelectMode `json:"mode,omitempty"`

	// Generation selects the snapshot captured at this exact Cluster
	// generation. Used when mode is Generation.
	// +optional
	Generation *int64 `json:"generation,omitempty"`

	// Timestamp selects the newest snapshot captured at or before this time.
	// Used when mode is Timestamp.
	// +optional
	Timestamp *metav1.Time `json:"timestamp,omitempty"`

	// OnUnresolvable is what to do when mode is AlignWithRecoveryTarget but the
	// target cannot be mapped to a moment — recoveryTarget.targetXID,
	// targetLSN, targetName and targetImmediate say nothing about wall-clock
	// time without replaying WAL.
	//
	// Fail is the default because the alternative is a cluster whose data and
	// configuration silently disagree.
	// +kubebuilder:default:=Fail
	// +optional
	OnUnresolvable UnresolvableAction `json:"onUnresolvable,omitempty"`
}

// SkipRule excludes part of a snapshot from being restored.
//
// A rule names either a capture group or a path, never both. A path skips
// itself and everything beneath it, so "skip every GUC" is
// `spec.postgresql.parameters` while skipping one is
// `spec.postgresql.parameters.work_mem`. A trailing ".*" is accepted and
// changes nothing.
//
// A path naming a key inside a map matches that key exactly. Label keys and
// GUC names may themselves contain dots, so `metadata.annotations.example`
// skips an annotation named "example" and leaves "example.com/owner" alone.
//
// When the path names a map itself — metadata.labels, metadata.annotations,
// spec.postgresql.parameters and the like — KeyPrefix and Except narrow the
// rule to some of its keys:
//
//	# No labels at all.
//	- path: metadata.labels
//
//	# Only these annotations.
//	- path: metadata.annotations
//	  except: [example.com/owner, team]
//
//	# Every annotation Argo CD owns.
//	- path: metadata.annotations
//	  keyPrefix: argocd.argoproj.io/
//
// Each rule removes a set of keys, and Except exempts keys from that rule
// only. When several rules apply, everything any of them removes is skipped.
// +kubebuilder:validation:XValidation:rule="has(self.group) != has(self.path)",message="a skip rule sets either group or path, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.group) || (!has(self.keyPrefix) && !has(self.except))",message="keyPrefix and except apply to a path naming a map, not to a group"
type SkipRule struct {
	// Group is a capture group name, such as Resources or Storage.
	// +optional
	Group string `json:"group,omitempty"`

	// Path is a dotted path into the Cluster.
	// +optional
	Path string `json:"path,omitempty"`

	// KeyPrefix narrows a rule on a map to the keys starting with this string,
	// matched against the whole key: `argocd.argoproj.io/` matches
	// `argocd.argoproj.io/tracking-id`.
	// +kubebuilder:validation:MinLength=1
	// +optional
	KeyPrefix string `json:"keyPrefix,omitempty"`

	// Except lists keys of the map this rule leaves alone, each matched as a
	// whole key.
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	Except []string `json:"except,omitempty"`
}

// TransformRule adjusts a captured value on its way into the restored Cluster.
//
// The expression is CEL, the same language Kubernetes uses for CRD validation
// rules. `old` is bound to the captured value, typed by its path: a quantity
// where CloudNativePG holds a resource quantity or a storage size, an integer
// for an integer field or a string holding a whole number, a double for a
// string holding a decimal, and a plain string for anything else. The result is
// written back in the captured value's own type.
//
//	spec.storage.size                             old.multiply(5)
//	spec.instances                                old > 3 ? 3 : old
//	spec.resources.limits.memory                  old.divide(2)
//	spec.postgresql.parameters.shared_buffers     pgScale(old, 2)
//	spec.postgresql.parameters.max_connections    old * 2
//
// Sizes use member calls rather than `old * 5` because cel-go binds arithmetic
// operators as singletons and will not accept overloads for a custom type.
// Plain integers take the operators directly.
//
// PostgreSQL memory settings are deliberately not treated as Kubernetes
// quantities: PostgreSQL's MB is binary while Kubernetes' M is decimal, so
// pgScale and pgMem exist for those.
type TransformRule struct {
	// Path is the dotted path whose value the expression rewrites. It must name
	// a value the snapshot actually captured, and must not also be skipped.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// OnMissing is what to do when the snapshot holds no value at this path.
	//
	// Fail, the default, because absence is usually a mistake this cannot tell
	// apart from a real one. Path spelling is checked against the capture
	// allowlist and CloudNativePG's types when the policy is applied, but that
	// check stops at a map — a mistyped GUC name or annotation key looks exactly
	// like a field the source cluster never set, and silently restoring
	// production's shared_buffers is worse than refusing the cluster.
	//
	// Ignore is for a path that is legitimately absent from some histories.
	// Capture groups gain paths as CloudNativePG gains fields, and snapshots are
	// kept far longer than that takes, so a transform that is correct today can
	// name something a snapshot from last year never carried — which is most
	// likely to bite during an AlignWithRecoveryTarget restore, where the whole
	// point is to reach for an old one.
	//
	// It covers absence only. A path that is also skipped, or that names an
	// object whose values would restore untransformed, still fails: neither is
	// missing, and both mean the rule does not do what it says.
	// +kubebuilder:default:=Fail
	// +optional
	OnMissing MissingAction `json:"onMissing,omitempty"`

	// Expression is the CEL expression, evaluated with `old` bound to the captured
	// value.
	//
	// The length bound is the real defence against an expensive expression.
	// Evaluation is compiled with a cost limit, but with `old` a single scalar
	// and no comprehensions in the environment, cost stays linear in the size of
	// the expression — so bounding the input is what bounds the work, and it
	// does so at the API server before the admission webhook is ever reached.
	// Real transforms are well under a hundred characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Expression string `json:"expression"`
}

// RestorePolicySpec describes what to restore and what to leave alone.
type RestorePolicySpec struct {
	// Select picks one snapshot from the history being restored.
	//
	// Unset means AlignWithRecoveryTarget when the Cluster bootstraps from a
	// recovery, and Latest otherwise. That default exists because the two cases
	// want opposite things: a cluster recovering data to last Tuesday wants the
	// configuration from last Tuesday, and a cluster starting fresh wants the
	// newest. Getting it wrong is silent, so it is not left to be configured.
	//
	// Note that Generation and Timestamp name a coordinate rather than a rule,
	// so a policy using either is meaningful for one history only. Latest and
	// AlignWithRecoveryTarget are rules and travel anywhere.
	// +optional
	Select RestoreSelect `json:"select,omitempty"`

	// Skip excludes parts of the snapshot. Anything skipped keeps whatever the
	// Cluster manifest specified, or the operator's default.
	// +optional
	Skip []SkipRule `json:"skip,omitempty"`

	// Transform rewrites captured values as they are restored, so a cluster can
	// inherit production's shape without inheriting its size.
	// +optional
	Transform []TransformRule `json:"transform,omitempty"`
}

// RestorePolicyStatus reports whether the policy's rules are usable.
//
// There is nothing here about snapshots or stores. A policy names no source —
// which history it is applied to is decided by the Cluster being created — so
// everything this controller can say is a property of the rules alone.
type RestorePolicyStatus struct {
	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=mdrestore;mdrestores
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.select.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RestorePolicy describes how a new Cluster inherits configuration from
// another cluster's captured history.
//
// It is consumed only at Cluster creation, by this plugin's mutating admission
// webhook. Editing it afterwards does not re-restore anything.
type RestorePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RestorePolicySpec `json:"spec,omitempty"`
	// +optional
	Status RestorePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestorePolicyList contains a list of RestorePolicy.
type RestorePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RestorePolicy `json:"items"`
}

// GetMode returns the select mode a policy states, or "" when there is no
// policy or it states none. The default is decided by ResolveMode, because it
// depends on the Cluster rather than on the policy.
func (r *RestorePolicy) GetMode() SelectMode {
	if r == nil {
		return ""
	}
	return r.Spec.Select.Mode
}

// ResolveMode returns the select mode to use for a restore.
//
// A policy is optional and may state no mode, so the default is decided here
// rather than on the policy: a Cluster bootstrapping from a recovery wants the
// configuration in force when that data was written, and one starting fresh
// wants the newest. Defaulting both to Latest would silently pair recovered
// data with configuration that never ran alongside it, which is the failure
// this project exists to prevent — so the default follows the Cluster.
func ResolveMode(policy *RestorePolicy, bootstrapsFromRecovery bool) SelectMode {
	if mode := policy.GetMode(); mode != "" {
		return mode
	}
	if bootstrapsFromRecovery {
		return SelectAlignWithRecoveryTarget
	}
	return SelectLatest
}

// GetOnUnresolvable returns the unresolvable-target policy, defaulting to Fail.
func (r *RestorePolicy) GetOnUnresolvable() UnresolvableAction {
	if r == nil || r.Spec.Select.OnUnresolvable == "" {
		return UnresolvableFail
	}
	return r.Spec.Select.OnUnresolvable
}
