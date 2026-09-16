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

package snapshot

import (
	"fmt"
	"reflect"
	"strings"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ValidateRulePath reports why a restore-policy path can never match anything,
// or nil when it might.
//
// Both questions it answers are static — they need the capture allowlist and
// CloudNativePG's own type definitions, never a snapshot. That is what lets a
// RestorePolicy be checked the moment it is written, even though a policy
// carries no source of its own and most never name one.
//
// It is deliberately not exhaustive. A path that survives this can still fail
// to match a particular history, because the source cluster never set an
// optional field; that residue is what checkAllFired reports at restore time.
func ValidateRulePath(path string) error {
	path = strings.TrimSuffix(path, ".*")
	if path == "" {
		return fmt.Errorf("a rule has an empty path")
	}

	if IsDeniedPath(path) {
		return fmt.Errorf(
			"%q is on the hard denylist, so it is never captured and never restored", path)
	}

	// Captured metadata is two maps of arbitrary user keys, so anything beneath
	// them is structurally valid and there is no schema to check it against.
	switch {
	case path == "metadata",
		path == "metadata.labels", strings.HasPrefix(path, "metadata.labels."),
		path == "metadata.annotations", strings.HasPrefix(path, "metadata.annotations."):
		return nil
	case strings.HasPrefix(path, "metadata."):
		return fmt.Errorf(
			"%q names Cluster metadata other than labels and annotations, which is never captured",
			path)
	}

	if !withinCaptureGroups(path) {
		return fmt.Errorf(
			"%q is covered by no capture group, so no snapshot can contain it", path)
	}
	return resolveInClusterSpec(path)
}

// withinCaptureGroups reports whether the allowlist could ever admit a path.
//
// Every group is considered, not just the default set: a store may enable an
// opt-in group, and a policy is written without knowing which store it will be
// used against. A path that merely contains a claimed path counts too, since
// skipping "spec" is coarse but coherent.
func withinCaptureGroups(path string) bool {
	for _, group := range Groups {
		for _, claimed := range group.Paths {
			if path == claimed ||
				strings.HasPrefix(path, claimed+".") ||
				strings.HasPrefix(claimed, path+".") {
				return true
			}
		}
	}
	return false
}

var (
	clusterSpecType          = reflect.TypeOf(apiv1.ClusterSpec{})
	quantityType             = reflect.TypeOf(resource.Quantity{})
	storageConfigurationType = reflect.TypeOf(apiv1.StorageConfiguration{})
)

// resolveInClusterSpec walks a dotted path through CloudNativePG's ClusterSpec.
//
// It stops at the first map, because a map admits any key and a key may itself
// contain dots: a path through spec.postgresql.parameters is checkable as far as
// "parameters" and no further, so a mistyped GUC name is not something this can
// catch.
func resolveInClusterSpec(path string) error {
	if path == "spec" {
		return nil
	}
	rest, ok := strings.CutPrefix(path, "spec.")
	if !ok {
		return fmt.Errorf("%q is not a path into a Cluster", path)
	}

	current := clusterSpecType
	walked := "spec"
	for _, segment := range strings.Split(rest, ".") {
		current = elemType(current)
		if current.Kind() == reflect.Map {
			return nil
		}
		if current.Kind() != reflect.Struct {
			return fmt.Errorf(
				"%q continues past %q, which holds a single value", path, walked)
		}

		next, ok := fieldByJSONName(current, segment)
		if !ok {
			return fmt.Errorf(
				"%q names no such field: CloudNativePG's %q has no %q", path, walked, segment)
		}
		current = next
		walked += "." + segment
	}
	return nil
}

// NamesMap reports whether a dotted path names a map itself — one whose keys
// are chosen by the user, such as metadata.labels or
// spec.postgresql.parameters — rather than a field or a key within one.
//
// A dotted path can be followed safely up to a map, because every segment
// before it is a Kubernetes field name and never contains a dot.
func NamesMap(path string) bool {
	if path == "metadata.labels" || path == "metadata.annotations" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "spec.")
	if !ok {
		return false
	}

	current := clusterSpecType
	for _, segment := range strings.Split(rest, ".") {
		current = elemType(current)
		if current.Kind() != reflect.Struct {
			return false
		}
		next, ok := fieldByJSONName(current, segment)
		if !ok {
			return false
		}
		current = next
	}
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	return current.Kind() == reflect.Map
}

// EnclosingMap returns the map a dotted path names or reaches into, and whether
// there is one. For "metadata.annotations.example.com/owner" it is
// "metadata.annotations", and everything after it is a single key.
func EnclosingMap(path string) (string, bool) {
	for i, char := range path {
		if char != '.' {
			continue
		}
		if prefix := path[:i]; NamesMap(prefix) {
			return prefix, true
		}
	}
	if NamesMap(path) {
		return path, true
	}
	return "", false
}

// IsQuantityPath reports whether CloudNativePG types the value at a path as a
// Kubernetes resource quantity, which decides how a transform sees it.
//
// Deciding by path rather than by the value is what keeps a policy consistent
// across histories. A CPU request of "2" and one of "500m" are both
// quantities, so one expression works on both; judged by the value alone, "2"
// would look like a plain number and old.multiply(2) would fail on that one
// cluster only.
//
// It takes segments rather than a dotted path because it runs on captured
// values, whose map keys are known to be whole — so unlike the rule check above
// it can follow a map to its element type.
//
// StorageConfiguration.Size is the one exception. CloudNativePG declares it a
// plain string, but it holds a size exactly like the quantities around it, and
// it is the value a transform most often rescales. Matching it by type covers
// storage, walStorage and every tablespace at once.
func IsQuantityPath(segments []string) bool {
	if len(segments) < 2 || segments[0] != "spec" {
		return false
	}

	current := clusterSpecType
	last := len(segments) - 2
	for i, segment := range segments[1:] {
		current = elemType(current)
		switch current.Kind() {
		case reflect.Map:
			current = current.Elem()
		case reflect.Struct:
			if current == storageConfigurationType && segment == "size" && i == last {
				return true
			}
			next, ok := fieldByJSONName(current, segment)
			if !ok {
				return false
			}
			current = next
		default:
			return false
		}
	}
	return elemType(current) == quantityType
}

// elemType looks through pointers and slices to the type they hold. A rule never
// indexes into a list, but a path may pass through one.
func elemType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t
}

// fieldByJSONName finds a struct field by its JSON name, descending into
// embedded structs whose fields are inlined rather than nested.
func fieldByJSONName(structType reflect.Type, name string) (reflect.Type, bool) {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		if tag == name {
			return field.Type, true
		}
		if tag == "" && field.Anonymous && field.Type.Kind() == reflect.Struct {
			if nested, ok := fieldByJSONName(field.Type, name); ok {
				return nested, true
			}
		}
	}
	return nil, false
}
