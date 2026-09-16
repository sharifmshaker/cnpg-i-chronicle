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
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
)

// ObjectStoreConfiguration describes how to reach an object store, and nothing
// more.
//
// This deliberately does not embed barman-cloud's BarmanObjectStoreConfiguration.
// That type is shaped for a backup tool: it carries WAL and base-backup
// compression, encryption, parallelism, command-line arguments and object tags.
// This plugin writes small JSON documents and reads them back, so none of that
// applies — yet embedding it would put every one of those fields in the CRD
// schema and in `kubectl explain`, presenting settings that are silently
// ignored. A field that looks configurable and does nothing is worse than one
// that is absent.
//
// The JSON field names match barman-cloud's, so a credentials block can still be
// copied across from an existing backup configuration unchanged.
//
// Only S3-compatible stores are implemented. Adding Azure Blob Storage or Google
// Cloud Storage is additive: a new optional credentials field here, a case in
// Resolver.Backend, an implementation of the five-method Backend interface, a
// branch in deriveConfiguration, and a wider destinationPath pattern. Nothing
// about that changes the shape of what already exists — which is why
// s3Credentials is an optional pointer under an exactly-one-of rule rather than
// a required struct.
// +kubebuilder:validation:XValidation:rule="has(self.s3Credentials)",message="exactly one credential block must be set; s3Credentials is currently the only supported kind"
type ObjectStoreConfiguration struct {
	// DestinationPath is the object store root, for example s3://backups/ or
	// s3://backups/some/prefix.
	//
	// The pattern admits only s3:// today. Widening it to azure:// or gs:// when
	// those backends land is backward compatible; narrowing would not be, which
	// is why it starts strict.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^s3://.+`
	DestinationPath string `json:"destinationPath"`

	// EndpointURL points at a self-hosted S3-compatible endpoint, such as
	// RustFS. Leave it unset for AWS S3 itself.
	//
	// Setting it also switches the client to path-style addressing, which
	// self-hosted endpoints require: virtual-host addressing needs per-bucket
	// DNS they do not provide.
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`

	// EndpointCA is a Secret key holding a PEM bundle for an endpoint serving a
	// private TLS certificate.
	// +optional
	EndpointCA *machineryapi.SecretKeySelector `json:"endpointCA,omitempty"`

	// S3Credentials authenticates to an S3-compatible store.
	//
	// Optional in the schema so that a sibling credential type can be added
	// without changing this field. The exactly-one-of rule on the enclosing type
	// is what makes it effectively required today.
	// +optional
	S3Credentials *S3Credentials `json:"s3Credentials,omitempty"`
}

// S3Credentials authenticates against an S3-compatible object store.
//
// Every field is read. Either set InheritFromIAMRole, or supply an access key
// and secret.
// +kubebuilder:validation:XValidation:rule="(has(self.inheritFromIAMRole) && self.inheritFromIAMRole) || (has(self.accessKeyId) && has(self.secretAccessKey))",message="set inheritFromIAMRole, or supply both accessKeyId and secretAccessKey"
type S3Credentials struct {
	// AccessKeyID is a Secret key holding the access key id.
	// +optional
	AccessKeyID *machineryapi.SecretKeySelector `json:"accessKeyId,omitempty"`

	// SecretAccessKey is a Secret key holding the secret access key.
	// +optional
	SecretAccessKey *machineryapi.SecretKeySelector `json:"secretAccessKey,omitempty"`

	// Region is a Secret key holding the region. Most S3-compatible endpoints
	// ignore it; AWS does not.
	// +optional
	Region *machineryapi.SecretKeySelector `json:"region,omitempty"`

	// SessionToken is a Secret key holding a session token, for temporary
	// credentials.
	// +optional
	SessionToken *machineryapi.SecretKeySelector `json:"sessionToken,omitempty"`

	// InheritFromIAMRole takes credentials from the ambient chain — IRSA, an
	// instance profile — instead of from Secrets.
	// +optional
	InheritFromIAMRole bool `json:"inheritFromIAMRole,omitempty"`
}
