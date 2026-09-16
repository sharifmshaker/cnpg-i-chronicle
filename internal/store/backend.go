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
	"errors"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// ErrBucketNotFound is returned by Check when the bucket or container does not
// exist yet. It is not a failure: Put creates it on first write.
var ErrBucketNotFound = errors.New("bucket not found")

// Backend is the minimal object-storage surface the plugin needs.
//
// It is deliberately small. The barman-cloud Go module supplies the credential
// and configuration schema, but not a storage client: barman shells out to the
// Python barman-cloud-* CLIs rather than talking to an SDK. Since the plugin
// only needs to read and write whole JSON documents, wrapping a cloud SDK
// behind five methods is far less machinery than invoking those CLIs would be.
type Backend interface {
	// Check proves the store is reachable with these credentials, using the
	// cheapest request that needs a permission capture already needs. It
	// returns ErrBucketNotFound when the bucket does not exist yet.
	//
	// prefix is the destination path within the bucket, because a policy may
	// grant listing only beneath it.
	Check(ctx context.Context, prefix string) error

	// Get returns the object at key, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Put writes data at key, overwriting any existing object.
	Put(ctx context.Context, key string, data []byte) error

	// List returns every key under prefix, in lexical order.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes the object at key. Removing a missing object is not an error.
	Delete(ctx context.Context, key string) error
}
