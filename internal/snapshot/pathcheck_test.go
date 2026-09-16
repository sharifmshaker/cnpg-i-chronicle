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
	"strings"
	"testing"
)

// Every path a person could reasonably write, and the reason each is or is not
// usable. The accepted half is taken from the examples in TransformRule's own
// documentation, so those cannot drift into being rejected.
func TestValidateRulePath(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want string // substring of the expected error, empty when it should pass
	}{
		// Documented transform examples.
		{path: "spec.storage.size"},
		{path: "spec.instances"},
		{path: "spec.resources.limits.memory"},
		{path: "spec.postgresql.parameters.shared_buffers"},

		// Valid in other shapes.
		{path: "spec.primaryLease.leaseDurationSeconds"},
		{path: "spec.postgresql.parameters.*"},
		{path: "spec.storage"},
		{path: "metadata.labels"},
		{path: "metadata.annotations.owner"},
		{path: "metadata"},

		// Misspelled beneath a subtree a group claims: the allowlist cannot see
		// it, so the type walk is what catches it.
		{path: "spec.storage.sizee", want: "no such field"},
		{path: "spec.instances.count", want: "holds a single value"},

		// Misspelled at the top level, where the allowlist answers first.
		{path: "spec.nonsense.foo", want: "no capture group"},

		// Real fields the allowlist does not carry.
		{path: "spec.bootstrap.initdb.database", want: "denylist"},
		{path: "spec.plugins", want: "denylist"},
		{path: "spec.replica.source", want: "denylist"},
		{path: "status.phase", want: "denylist"},

		// Metadata that is never captured.
		{path: "metadata.ownerReferences", want: "denylist"},
		{path: "metadata.finalizers", want: "denylist"},

		{path: "", want: "empty path"},
	} {
		err := ValidateRulePath(testCase.path)
		switch {
		case testCase.want == "" && err != nil:
			t.Errorf("%q should be usable, got: %v", testCase.path, err)
		case testCase.want != "" && err == nil:
			t.Errorf("%q should be rejected for %q, got nil", testCase.path, testCase.want)
		case testCase.want != "" && !strings.Contains(err.Error(), testCase.want):
			t.Errorf("%q: want error mentioning %q, got: %v", testCase.path, testCase.want, err)
		}
	}
}

// Validation has to stop at a map, because a map takes any key. Asserting it
// here keeps the limit deliberate: someone reading the checks should not come
// away believing a mistyped GUC name is caught.
func TestValidateRulePathStopsAtMaps(t *testing.T) {
	for _, path := range []string{
		"spec.postgresql.parameters.not_a_real_guc",
		"spec.resources.limits.not_a_real_resource",
		"metadata.labels.anything",
	} {
		if err := ValidateRulePath(path); err != nil {
			t.Errorf("%q is beneath a map and cannot be checked, so it must pass: %v", path, err)
		}
	}
}

func TestIsQuantityPath(t *testing.T) {
	for _, testCase := range []struct {
		segments []string
		want     bool
	}{
		// resource.Quantity in CloudNativePG's types, including through a map.
		{[]string{"spec", "resources", "requests", "cpu"}, true},
		{[]string{"spec", "resources", "limits", "memory"}, true},
		{[]string{"spec", "ephemeralVolumesSizeLimit", "shm"}, true},
		{[]string{"spec", "probes", "readiness", "maximumLag"}, true},

		// StorageConfiguration.Size, a string CloudNativePG does not type as one.
		{[]string{"spec", "storage", "size"}, true},
		{[]string{"spec", "walStorage", "size"}, true},

		// Everything else.
		{[]string{"spec", "postgresql", "parameters", "shared_buffers"}, false},
		{[]string{"spec", "postgresql", "parameters", "pgaudit.log"}, false},
		{[]string{"spec", "instances"}, false},
		{[]string{"spec", "storage", "storageClass"}, false},
		{[]string{"spec", "resources"}, false},
		{[]string{"spec", "resources", "requests", "cpu", "deeper"}, false},
		{[]string{"spec", "noSuchField"}, false},
		{[]string{"metadata", "labels", "size"}, false},
		{nil, false},
	} {
		if got := IsQuantityPath(testCase.segments); got != testCase.want {
			t.Errorf("IsQuantityPath(%v) = %v, want %v", testCase.segments, got, testCase.want)
		}
	}
}

func TestNamesMapAndEnclosingMap(t *testing.T) {
	for path, want := range map[string]bool{
		"metadata.labels":                 true,
		"metadata.annotations":            true,
		"spec.postgresql.parameters":      true,
		"spec.resources.requests":         true,
		"spec.postgresql":                 false,
		"spec.storage.size":               false,
		"spec.postgresql.parameters.x":    false,
		"metadata.annotations.example.io": false,
		"spec.noSuchField":                false,
	} {
		if got := NamesMap(path); got != want {
			t.Errorf("NamesMap(%q) = %v, want %v", path, got, want)
		}
	}

	for path, want := range map[string]string{
		"metadata.annotations.example.com/owner": "metadata.annotations",
		"spec.postgresql.parameters.pgaudit.log": "spec.postgresql.parameters",
		"spec.postgresql.parameters":             "spec.postgresql.parameters",
		"spec.storage.size":                      "",
	} {
		got, _ := EnclosingMap(path)
		if got != want {
			t.Errorf("EnclosingMap(%q) = %q, want %q", path, got, want)
		}
	}
}
