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
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestDumpSnapshotShape is a documentation guard: it prints the real serialized
// form so the format in the README cannot drift from the code unnoticed.
func TestDumpSnapshotShape(t *testing.T) {
	snap, err := Extract(realisticCluster(), ExtractOptions{
		PluginVersion: "0.1.0",
		Now:           time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	// The embedded Content must serialize flat, not nested under "Content".
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatal(err)
	}
	if _, nested := asMap["Content"]; nested {
		t.Error("Content is nested instead of inlined; the embedded struct tag is wrong")
	}
	for _, key := range []string{"apiVersion", "kind", "capturedAt", "generation", "spec", "checksum"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("snapshot document is missing top-level %q", key)
		}
	}

	if out := os.Getenv("DUMP_SNAPSHOT"); out != "" {
		if err := os.WriteFile(out, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
