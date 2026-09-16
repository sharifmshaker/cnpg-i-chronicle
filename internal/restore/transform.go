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

package restore

import (
	"fmt"
	"sort"
	"strings"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/restore/celenv"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

// TransformSet holds the compiled expressions for a restore policy.
type TransformSet struct {
	byPath map[string]compiledTransform
	order  []string
}

// compiledTransform is one rule, ready to run.
type compiledTransform struct {
	program *celenv.Program

	// onMissing is carried per rule rather than per policy: whether a path may
	// legitimately be absent is a property of that path, not of the policy.
	onMissing chroniclev1.MissingAction
}

// NewTransformSet compiles every transform in a policy.
//
// Compiling up front means a typo in an expression is reported when the policy
// is applied, not when someone is waiting on a cluster to be created.
func NewTransformSet(rules []chroniclev1.TransformRule) (*TransformSet, error) {
	if len(rules) == 0 {
		return &TransformSet{}, nil
	}

	env, err := celenv.New()
	if err != nil {
		return nil, fmt.Errorf("while building the expression environment: %w", err)
	}

	set := &TransformSet{byPath: map[string]compiledTransform{}}
	for _, rule := range rules {
		if rule.Path == "" {
			return nil, fmt.Errorf("a transform rule has no path")
		}
		if _, duplicate := set.byPath[rule.Path]; duplicate {
			return nil, fmt.Errorf("two transform rules target %q", rule.Path)
		}

		program, err := celenv.Compile(env, rule.Expression)
		if err != nil {
			return nil, fmt.Errorf("transform for %q is not a valid expression: %w", rule.Path, err)
		}
		onMissing := rule.OnMissing
		if onMissing == "" {
			// A policy built in Go rather than decoded from the API server sees
			// no kubebuilder default, and the safe reading of "unset" is Fail.
			onMissing = chroniclev1.MissingFail
		}
		set.byPath[rule.Path] = compiledTransform{program: program, onMissing: onMissing}
		set.order = append(set.order, rule.Path)
	}
	sort.Strings(set.order)
	return set, nil
}

// Paths returns the transformed paths, sorted.
func (t *TransformSet) Paths() []string {
	if t == nil {
		return nil
	}
	return append([]string(nil), t.order...)
}

// Program returns the compiled expression for a path.
func (t *TransformSet) Program(path string) (*celenv.Program, bool) {
	if t == nil {
		return nil, false
	}
	compiled, ok := t.byPath[path]
	if !ok {
		return nil, false
	}
	return compiled.program, true
}

// toleratesMissing reports whether this path is allowed to be absent from the
// snapshot being restored.
func (t *TransformSet) toleratesMissing(path string) bool {
	if t == nil {
		return false
	}
	return t.byPath[path].onMissing == chroniclev1.MissingIgnore
}

// Apply evaluates the transform for a captured value, if its path has one.
//
// It takes the leaf rather than a dotted path because how `old` is typed is
// decided by the path's type in CloudNativePG's API, and that walk needs the
// segments whole: a GUC or label key may itself contain dots.
func (t *TransformSet) Apply(leaf pathutil.Leaf) (any, bool, error) {
	path := leaf.Path()
	program, ok := t.Program(path)
	if !ok {
		return leaf.Value, false, nil
	}

	result, err := program.Evaluate(leaf.Value, snapshot.IsQuantityPath(leaf.Segments))
	if err != nil {
		return nil, true, fmt.Errorf("transforming %s: %w", path, err)
	}
	return result, true, nil
}

// checkAllFired reports transforms that never matched anything.
//
// A transform that silently does nothing is almost always a mistake, but the
// three shapes it takes bite differently and are worth telling apart:
//
//   - Both transformed and skipped, which contradicts itself.
//   - A path naming an object rather than a value. This is the dangerous one:
//     the values beneath it are still restored, untransformed, so a cluster
//     comes up at production's size because the expression meant to shrink it
//     was written one level too high.
//   - A path the snapshot does not hold. CompileRules already rejects the
//     ones that are wrong on their face, so what reaches here is either a
//     mistyped map key — a GUC name, an annotation — which no static check can
//     see, or an optional field the source cluster never set.
//
// The first two always fail: neither is about absence, and both mean the rule
// does not do what it says. The third is the one a rule can opt out of with
// onMissing: Ignore, because a mistyped GUC name and a field the source cluster
// never set are indistinguishable here — so the default stays Fail and the
// author says which one it is.
func (t *TransformSet) checkAllFired(
	fired map[string]bool,
	skipped map[string]string,
	leaves []string,
) error {
	var problems []string
	for _, path := range t.Paths() {
		if fired[path] {
			continue
		}
		if reason, wasSkipped := skipped[path]; wasSkipped {
			problems = append(problems, fmt.Sprintf(
				"%s is both transformed and skipped (%s); a skipped path is never restored, "+
					"so the transform cannot run", path, reason))
			continue
		}
		if beneath := countBeneath(leaves, path); beneath > 0 {
			problems = append(problems, fmt.Sprintf(
				"%s names an object, not a value: the %d value(s) beneath it would be "+
					"restored untransformed, so transform those paths instead", path, beneath))
			continue
		}
		if t.toleratesMissing(path) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s is transformed but the snapshot captured no such value "+
				"(set onMissing: Ignore if this path is legitimately absent from "+
				"older snapshots)", path))
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// countBeneath counts the snapshot's values that live under a path, which is
// what separates "you aimed at an object" from "there is nothing there".
func countBeneath(leaves []string, path string) int {
	count := 0
	for _, leaf := range leaves {
		if strings.HasPrefix(leaf, path+".") {
			count++
		}
	}
	return count
}
