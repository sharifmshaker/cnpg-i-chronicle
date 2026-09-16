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

// Package pathutil provides dotted-path access over the generic JSON
// representation of a Kubernetes object.
//
// Capture and restore both operate on dotted paths rather than typed struct
// fields. That keeps a single vocabulary across the whole plugin: the same
// "spec.postgresql.parameters.work_mem" string names a captured field, a skip
// rule and a transform target, and none of it has to be kept in sync with
// CloudNativePG's Go types as they evolve.
//
// Two families of functions exist because a dotted path cannot always be
// split. Kubernetes field names never contain dots, but the two places a map
// key is user-chosen — PostgreSQL namespaced GUCs such as "pgaudit.log", and
// label and annotation keys such as "example.com/owner" — routinely do.
// Splitting those would silently build nested objects where a single key was
// meant. The dotted helpers (Get, Set, Delete) are for paths that are known to
// be splittable; the segment helpers take the split already done, so a caller
// that knows a key is one segment can say so.
package pathutil

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Split breaks a dotted path into its segments.
func Split(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
}

// Get returns the value at path, and whether it was present.
func Get(obj map[string]any, path string) (any, bool) {
	return GetSegments(obj, Split(path))
}

// Set writes value at path, creating intermediate maps as needed. It returns
// false if an existing non-map value blocks the path.
func Set(obj map[string]any, path string, value any) bool {
	return SetSegments(obj, Split(path), value)
}

// Delete removes the value at path. Intermediate maps left empty are pruned, so
// that removing the last captured field under a section does not leave an empty
// object behind to churn the snapshot checksum.
func Delete(obj map[string]any, path string) {
	segments := Split(path)
	if len(segments) == 0 {
		return
	}
	deleteSegments(obj, segments)
}

func deleteSegments(current map[string]any, segments []string) {
	if len(segments) == 1 {
		delete(current, segments[0])
		return
	}

	next, ok := current[segments[0]].(map[string]any)
	if !ok {
		return
	}
	deleteSegments(next, segments[1:])
	if len(next) == 0 {
		delete(current, segments[0])
	}
}

// GetSegments returns the value at the pre-split path, and whether it was
// present.
func GetSegments(obj map[string]any, segments []string) (any, bool) {
	if len(segments) == 0 {
		return nil, false
	}

	var current any = obj
	for _, segment := range segments {
		asMap, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = asMap[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// SetSegments writes value at the pre-split path, creating intermediate maps as
// needed. It returns false if an existing non-map value blocks the path.
func SetSegments(obj map[string]any, segments []string, value any) bool {
	if len(segments) == 0 {
		return false
	}

	current := obj
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment]
		if !ok || next == nil {
			created := map[string]any{}
			current[segment] = created
			current = created
			continue
		}
		asMap, ok := next.(map[string]any)
		if !ok {
			return false
		}
		current = asMap
	}
	current[segments[len(segments)-1]] = value
	return true
}

// ToMap converts a Kubernetes object into its generic JSON form.
//
// Numbers are decoded as json.Number rather than float64. That preserves the
// exact source representation, which matters twice over: int64 generations
// survive a round trip intact, and re-marshalling is byte-stable, so the
// snapshot checksum does not drift for reasons that have nothing to do with
// the cluster's configuration.
func ToMap(object any) (map[string]any, error) {
	raw, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return FromJSON(raw)
}

// FromJSON decodes JSON into a generic map, preserving number representation.
func FromJSON(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// Leaf is one terminal value with its path already split into segments.
type Leaf struct {
	// Segments is the path, split. The final segment may contain dots.
	Segments []string

	// Value is the terminal value: a scalar, a slice, or an empty map.
	Value any
}

// Path renders a leaf's segments as a dotted string, for display and for
// matching against user-written skip rules. It is not safe to split again.
func (l Leaf) Path() string { return strings.Join(l.Segments, ".") }

// WalkLeaves returns every terminal value in obj, with paths kept as segments.
//
// Slices are terminal: the plugin restores whole lists rather than indexing
// into them, because a positional path into a list stops meaning anything once
// the list is reordered.
func WalkLeaves(obj map[string]any) []Leaf {
	var out []Leaf
	walkLeaves(obj, nil, &out)
	return out
}

func walkLeaves(current map[string]any, prefix []string, out *[]Leaf) {
	for key, value := range current {
		segments := append(append([]string(nil), prefix...), key)
		if nested, ok := value.(map[string]any); ok && len(nested) > 0 {
			walkLeaves(nested, segments, out)
			continue
		}
		*out = append(*out, Leaf{Segments: segments, Value: value})
	}
}
