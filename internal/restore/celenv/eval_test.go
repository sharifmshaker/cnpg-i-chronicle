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

package celenv

import (
	"strings"
	"testing"
)

// An expression pathological enough to be a problem fails to compile, which is
// what keeps it out of the admission path entirely. Pinned because the reasoning
// behind MaxCost depends on it: if cel-go ever accepts these, the cost limit
// stops being a backstop and becomes the only thing standing there.
func TestPathologicalExpressionIsRejectedAtCompileTime(t *testing.T) {
	env, err := New()
	if err != nil {
		t.Fatal(err)
	}

	// Deep enough to exceed cel-go's parser recursion limit.
	if _, err := Compile(env, "old"+strings.Repeat(" + old", 800)); err == nil {
		t.Error("a deeply nested expression compiled; MaxCost is now the only guard")
	}

	// And a merely long one still evaluates predictably rather than hanging.
	program, err := Compile(env, "old"+strings.Repeat(" + old", 50))
	if err != nil {
		t.Fatalf("a reasonable expression should still compile: %v", err)
	}
	if _, err := program.Evaluate("abc", false); err != nil {
		t.Errorf("evaluation failed: %v", err)
	}
}
