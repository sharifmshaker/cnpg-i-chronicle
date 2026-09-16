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
	"testing"
)

// "Empty means the default set" is a rule several callers depend on; resolving
// it in one place is what keeps them agreeing.
func TestEffectiveGroupNames(t *testing.T) {
	if got := EffectiveGroupNames(nil); len(got) != len(DefaultGroupNames()) {
		t.Errorf("nil should resolve to the default set, got %v", got)
	}
	if got := EffectiveGroupNames([]string{}); len(got) != len(DefaultGroupNames()) {
		t.Errorf("empty should resolve to the default set, got %v", got)
	}
	got := EffectiveGroupNames([]string{"Gucs"})
	if len(got) != 1 || got[0] != "Gucs" {
		t.Errorf("a selection should be returned as given, got %v", got)
	}
	// The opt-in groups must not be in the default set, or they are not opt-in.
	for _, name := range DefaultGroupNames() {
		if name == "Identity" || name == "Secrets" {
			t.Errorf("%s is off by default and must not appear in DefaultGroupNames", name)
		}
	}
}
