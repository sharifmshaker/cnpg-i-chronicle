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

package store

import (
	"context"
	"fmt"

	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

// Resolved is a ConfigStore reduced to everything needed to talk to storage.
type Resolved struct {
	// Configuration is the effective object store configuration.
	Configuration *chroniclev1.ObjectStoreConfiguration

	// Destination is the parsed destinationPath.
	Destination Destination

	// Source records the provenance: "Inline" or "ObjectStore/<name>".
	Source string

	// Provider is s3, azure or google.
	Provider string

	// InheritedRetention is retentionPolicy from the barman ObjectStore this
	// store derives from, empty when there is none or the configuration is
	// inline. Read only to serve retention: InheritFromObjectStore; barman owns
	// what it means for backups.
	InheritedRetention string
}

// Resolver turns a ConfigStore into a usable Backend.
type Resolver struct {
	// Client reads Secrets holding credentials.
	Client client.Reader

	// ObjectStores resolves spec.derivedFrom.
	ObjectStores *ObjectStoreReader
}

// Resolve produces the effective configuration for a store.
func (r *Resolver) Resolve(
	ctx context.Context,
	store *chroniclev1.ConfigStore,
) (*Resolved, error) {
	var (
		configuration      *chroniclev1.ObjectStoreConfiguration
		source             string
		inheritedRetention string
	)

	switch {
	case store.Spec.DerivedFrom != nil:
		key := types.NamespacedName{
			Namespace: store.Namespace,
			Name:      store.Spec.DerivedFrom.Name,
		}
		found, err := r.ObjectStores.GetConfiguration(ctx, key)
		if err != nil {
			return nil, err
		}
		configuration = found.Configuration
		inheritedRetention = found.RetentionPolicy
		source = "ObjectStore/" + store.Spec.DerivedFrom.Name

	case store.Spec.Configuration != nil:
		configuration = store.Spec.Configuration
		source = "Inline"

	default:
		// The CRD's CEL rule makes this unreachable through the API server, but
		// objects constructed in tests bypass admission.
		return nil, fmt.Errorf(
			"ConfigStore %s/%s sets neither derivedFrom nor configuration",
			store.Namespace, store.Name)
	}

	destination, err := ParseDestination(configuration.DestinationPath)
	if err != nil {
		return nil, fmt.Errorf("ConfigStore %s/%s: %w", store.Namespace, store.Name, err)
	}

	return &Resolved{
		Configuration:      configuration,
		Destination:        destination,
		Source:             source,
		Provider:           "s3",
		InheritedRetention: inheritedRetention,
	}, nil
}

// Backend builds a storage client for a resolved store.
//
// A new client is built on every call, reading its Secrets afresh, and that is
// deliberate. Each build costs a few uncached Secret reads, a TLS handshake,
// and with inheritFromIAMRole an STS round trip. But the callers sit behind
// fast paths that almost always answer without a client at all: capture and
// status build one only when a Cluster's configuration actually moved, restore
// once per Cluster created, and retention once per store per resync. A cache
// would save little, and it would cost a rotated Secret taking effect late, or
// the eviction-on-auth-failure logic needed to avoid that.
func (r *Resolver) Backend(
	ctx context.Context,
	namespace string,
	resolved *Resolved,
) (Backend, error) {
	switch resolved.Destination.Scheme {
	case "s3":
		return r.s3Backend(ctx, namespace, resolved)
	case "azure", "gs":
		// Deliberately explicit rather than silently degrading: a cluster whose
		// snapshots appear to be configured but are never written would be worse
		// than a startup failure.
		return nil, fmt.Errorf(
			"object store scheme %q is not supported yet; only s3:// is implemented",
			resolved.Destination.Scheme)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", resolved.Destination.Scheme)
	}
}

func (r *Resolver) s3Backend(
	ctx context.Context,
	namespace string,
	resolved *Resolved,
) (Backend, error) {
	options := S3Options{
		Bucket:      resolved.Destination.Bucket,
		EndpointURL: resolved.Configuration.EndpointURL,
	}

	credentials := resolved.Configuration.S3Credentials
	if credentials == nil {
		return nil, fmt.Errorf(
			"the object store configuration has no s3Credentials; an S3 destination needs them")
	}
	options.InheritFromIAMRole = credentials.InheritFromIAMRole

	if !credentials.InheritFromIAMRole {
		var err error
		if options.AccessKeyID, err = r.secretValue(ctx, namespace, credentials.AccessKeyID); err != nil {
			return nil, fmt.Errorf("while reading s3Credentials.accessKeyId: %w", err)
		}
		if options.SecretAccessKey, err = r.secretValue(ctx, namespace, credentials.SecretAccessKey); err != nil {
			return nil, fmt.Errorf("while reading s3Credentials.secretAccessKey: %w", err)
		}
		if options.SessionToken, err = r.secretValue(ctx, namespace, credentials.SessionToken); err != nil {
			return nil, fmt.Errorf("while reading s3Credentials.sessionToken: %w", err)
		}
	}

	region, err := r.secretValue(ctx, namespace, credentials.Region)
	if err != nil {
		return nil, fmt.Errorf("while reading s3Credentials.region: %w", err)
	}
	options.Region = region

	if ca := resolved.Configuration.EndpointCA; ca != nil {
		bundle, err := r.secretValue(ctx, namespace, ca)
		if err != nil {
			return nil, fmt.Errorf("while reading endpointCA: %w", err)
		}
		options.CABundle = []byte(bundle)
	}

	return NewS3Backend(ctx, options)
}

// secretValue reads one key out of a Secret. A nil selector yields "".
func (r *Resolver) secretValue(
	ctx context.Context,
	namespace string,
	selector *machineryapi.SecretKeySelector,
) (string, error) {
	if selector == nil || selector.Name == "" {
		return "", nil
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: selector.Name}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("while reading Secret %s: %w", key, err)
	}

	value, ok := secret.Data[selector.Key]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q", key, selector.Key)
	}
	return string(value), nil
}

// LayoutFor builds the key layout for one cluster within a store.
func LayoutFor(store *chroniclev1.ConfigStore, resolved *Resolved, serverName string) Layout {
	return Layout{
		BasePath:   resolved.Destination.Path,
		ServerName: serverName,
		Prefix:     store.GetPrefix(),
	}
}

// Provider resolves a ConfigStore into a usable object-storage client.
//
// Both the reconciler hook and the restore webhook depend on this rather than
// on *Resolver directly, so each can be exercised against an in-memory backend.
// *Resolver is the only production implementation.
type Provider interface {
	Resolve(ctx context.Context, configStore *chroniclev1.ConfigStore) (*Resolved, error)
	Backend(ctx context.Context, namespace string, resolved *Resolved) (Backend, error)
}

var _ Provider = (*Resolver)(nil)
