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

package pathutil

import "testing"

func TestGetSetDelete(t *testing.T) {
	object := map[string]any{}

	if ok := Set(object, "spec.storage.size", "10Gi"); !ok {
		t.Fatal("Set should have created intermediate maps")
	}
	value, found := Get(object, "spec.storage.size")
	if !found || value != "10Gi" {
		t.Fatalf("Get = %v, %v; want 10Gi, true", value, found)
	}

	// Deleting the only leaf should prune the empty parents it leaves behind,
	// otherwise an empty {"spec":{"storage":{}}} would perturb the checksum.
	Delete(object, "spec.storage.size")
	if len(object) != 0 {
		t.Fatalf("empty parents were not pruned: %#v", object)
	}
}

func TestSetRefusesToOverwriteScalar(t *testing.T) {
	object := map[string]any{"spec": "not-a-map"}
	if Set(object, "spec.storage.size", "10Gi") {
		t.Fatal("Set should refuse to traverse through a scalar")
	}
}

func TestGetMissingPath(t *testing.T) {
	object := map[string]any{"spec": map[string]any{}}
	for _, path := range []string{"", "spec.missing", "spec.missing.deeper", "absent"} {
		if _, found := Get(object, path); found {
			t.Errorf("Get(%q) reported found on an absent path", path)
		}
	}
}

func TestToMapPreservesIntegerRepresentation(t *testing.T) {
	// float64 round-tripping would render a large generation in scientific
	// notation and silently corrupt the watermark comparison.
	type object struct {
		Generation int64 `json:"generation"`
	}
	asMap, err := ToMap(object{Generation: 9007199254740993})
	if err != nil {
		t.Fatal(err)
	}
	if got := asMap["generation"].(interface{ String() string }).String(); got != "9007199254740993" {
		t.Fatalf("generation = %s, want 9007199254740993", got)
	}
}

// Label keys and PostgreSQL namespaced GUCs contain dots. Treating them as
// paths would build nested objects where one key was meant.
func TestSegmentsHandleDottedKeys(t *testing.T) {
	object := map[string]any{}

	segments := []string{"spec", "postgresql", "parameters", "pgaudit.log"}
	if !SetSegments(object, segments, "all") {
		t.Fatal("SetSegments failed")
	}

	parameters := object["spec"].(map[string]any)["postgresql"].(map[string]any)["parameters"].(map[string]any)
	if got, ok := parameters["pgaudit.log"]; !ok || got != "all" {
		t.Fatalf("expected a single key \"pgaudit.log\", got %#v", parameters)
	}
	if _, nested := parameters["pgaudit"]; nested {
		t.Error("the dotted GUC name was split into nested maps")
	}

	got, found := GetSegments(object, segments)
	if !found || got != "all" {
		t.Errorf("GetSegments = %v, %v; want all, true", got, found)
	}

	// The dotted-path helper cannot see it, which is exactly why the segment
	// helpers exist.
	if _, found := Get(object, "spec.postgresql.parameters.pgaudit.log"); found {
		t.Error("dotted Get unexpectedly resolved a dotted key; the two APIs should differ here")
	}
}

func TestWalkLeavesKeepsDottedKeysWhole(t *testing.T) {
	object, err := FromJSON([]byte(`{
	  "metadata": {"annotations": {"example.com/owner": "dba", "plain": "x"}},
	  "spec": {"postgresql": {"parameters": {"pgaudit.log": "all"}}, "instances": 3}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]Leaf{}
	for _, leaf := range WalkLeaves(object) {
		byPath[leaf.Path()] = leaf
	}

	for path, wantLast := range map[string]string{
		"metadata.annotations.example.com/owner": "example.com/owner",
		"spec.postgresql.parameters.pgaudit.log": "pgaudit.log",
		"spec.instances":                         "instances",
	} {
		leaf, ok := byPath[path]
		if !ok {
			t.Errorf("missing leaf %q; got %v", path, byPath)
			continue
		}
		if last := leaf.Segments[len(leaf.Segments)-1]; last != wantLast {
			t.Errorf("%s final segment = %q, want %q", path, last, wantLast)
		}
	}
}
