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
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// TestGroupsCoverClusterSpec is what makes an allowlist maintainable.
//
// Capture is allowlist-first, and an allowlist fails silently in the safe
// direction: a field CloudNativePG adds in a new release is simply not
// captured, and nothing says so until someone notices a restored cluster
// missing a setting. This turns that silence into a failing test at the moment
// the api dependency is bumped — which is the moment someone should be deciding
// where the new field belongs. spec.primaryLease reached v1.30 and sat
// uncaptured exactly this way.
//
// Only top-level spec fields are checked. Groups claim whole subtrees, so
// "spec.postgresql" already covers anything added beneath it; a new top-level
// field is the case that goes unnoticed.
func TestGroupsCoverClusterSpec(t *testing.T) {
	claimed := map[string]string{}
	for _, group := range Groups {
		for _, path := range group.Paths {
			if head, ok := specHead(path); ok {
				claimed[head] = "group " + group.Name
			}
		}
	}
	for _, path := range DeniedPaths {
		if head, ok := specHead(path); ok {
			claimed[head] = "the denylist"
		}
	}

	var uncovered []string
	for _, field := range clusterSpecFields(t) {
		if _, ok := claimed[field]; !ok {
			uncovered = append(uncovered, field)
		}
	}

	if len(uncovered) > 0 {
		t.Errorf("ClusterSpec fields claimed by no capture group and no denylist "+
			"entry: %s\n"+
			"Add each to a Group in groups.go, or to DeniedPaths in denylist.go if it "+
			"must never cross clusters. Until then it is silently never captured and "+
			"never restored.", strings.Join(uncovered, ", "))
	}
}

// TestGroupPathsExist catches the same drift from the other side. A path that
// no longer names a real field captures nothing, and says nothing either, so an
// upstream rename would quietly empty out a group.
func TestGroupPathsExist(t *testing.T) {
	fields := map[string]bool{}
	for _, name := range clusterSpecFields(t) {
		fields[name] = true
	}

	check := func(source, path string) {
		head, ok := specHead(path)
		if !ok || fields[head] {
			return
		}
		t.Errorf("%s references %q, but ClusterSpec has no %q field: it was renamed "+
			"or removed upstream, and that path now captures nothing",
			source, path, head)
	}

	for _, group := range Groups {
		for _, path := range group.Paths {
			check("group "+group.Name, path)
		}
	}
	for _, path := range DeniedPaths {
		check("DeniedPaths", path)
	}
}

// specHead reduces "spec.postgresql.parameters" to "postgresql", and reports
// false for a path that is not under spec at all — metadata and status entries
// on the denylist are not ClusterSpec fields.
func specHead(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "spec.")
	if !ok {
		return "", false
	}
	head, _, _ := strings.Cut(rest, ".")
	return head, true
}

// clusterSpecFields lists the JSON names of ClusterSpec's top-level fields.
func clusterSpecFields(t *testing.T) []string {
	t.Helper()

	specType := reflect.TypeOf(apiv1.ClusterSpec{})
	names := make([]string, 0, specType.NumField())
	for i := 0; i < specType.NumField(); i++ {
		field := specType.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		// An embedded or untagged exported field would be invisible to the
		// check above, so it is reported rather than skipped.
		if name == "" || name == "-" {
			if field.IsExported() && name != "-" {
				t.Errorf("ClusterSpec.%s has no usable json tag; this test cannot "+
					"see it and neither can the path-based allowlist", field.Name)
			}
			continue
		}
		names = append(names, name)
	}
	return names
}
