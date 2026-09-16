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

// Package snapshot turns a CloudNativePG Cluster into a portable, versioned
// description of its configuration, and back again.
package snapshot

import (
	"time"
)

const (
	// APIVersion of the snapshot document format.
	APIVersion = "chronicle.sharifmshaker.github.io/v1"

	// Kind of the snapshot document.
	Kind = "ClusterMetadataSnapshot"
)

// ClusterRef identifies the cluster a snapshot was taken from.
type ClusterRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid,omitempty"`
}

// SourceInfo records what produced the snapshot, so a future reader can reason
// about format and field availability without guessing.
type SourceInfo struct {
	PluginVersion string `json:"pluginVersion"`
	ImageName     string `json:"imageName,omitempty"`
}

// Content is the part of a snapshot that describes configuration, and the only
// part the checksum covers.
//
// capturedAt and generation deliberately sit outside it: they change on every
// capture, so including them would make every snapshot look different from the
// last and defeat the "nothing actually changed" short circuit.
type Content struct {
	// Spec is the captured subset of the Cluster spec, as generic JSON.
	Spec map[string]any `json:"spec,omitempty"`

	// Labels and Annotations are the captured subset of Cluster metadata.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Snapshot is one captured configuration state of a Cluster.
type Snapshot struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	CapturedAt time.Time  `json:"capturedAt"`
	Generation int64      `json:"generation"`
	Cluster    ClusterRef `json:"cluster"`
	Source     SourceInfo `json:"source"`

	// Groups lists the capture groups that were enabled when this snapshot was
	// taken.
	//
	// Restore does not read it: it writes whatever paths the snapshot contains
	// and never clears a path it lacks, so "not captured" and "captured as
	// empty" produce the same result on the target. It makes an old snapshot
	// legible — a snapshot that predates enabling a group is genuinely missing
	// those fields rather than the source cluster having had none — and it is
	// an input to the capture watermark's fingerprint.
	Groups []string `json:"groups"`

	// CaptureRules is the CaptureRulesDigest this snapshot was captured under.
	//
	// The watermark's fingerprint is recomputed from the newest snapshot, so the
	// rules have to be recorded in it: a snapshot whose content is unchanged
	// but whose rules are not is superseded, which is how a plugin upgrade that
	// captures more gets written out without waiting for an unrelated edit.
	CaptureRules string `json:"captureRules"`

	Content `json:",inline"`

	// Checksum is sha256 over the canonical encoding of Content.
	Checksum string `json:"checksum"`
}
