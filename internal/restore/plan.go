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

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

// Change is one field the restore sets.
type Change struct {
	// Path is the dotted rendering, for display and for matching skip rules.
	Path string `json:"path"`

	// segments is the authoritative path. It is kept split because the final
	// segment may itself contain dots — "pgaudit.log" is one GUC name, and
	// "example.com/owner" is one annotation key. Re-splitting Path would turn
	// either into a nest of empty objects.
	segments []string

	// value is what to write.
	value any

	// transformed records that an expression produced this value.
	transformed bool
}

// Exclusion is one field the restore deliberately leaves alone.
type Exclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Plan is the fully resolved set of edits a restore will make, computed before
// anything is mutated so it can be logged, tested and explained.
type Plan struct {
	// Changes are the paths that will be set, sorted.
	Changes []Change

	// Exclusions are paths present in the snapshot that will not be set.
	Exclusions []Exclusion
}

// restorableTree reassembles a snapshot into the shape of a Cluster, so that
// every path is absolute and uses the same vocabulary as skip rules and the
// denylist.
func restorableTree(snap *snapshot.Snapshot) map[string]any {
	restorable := map[string]any{}
	if len(snap.Spec) > 0 {
		restorable["spec"] = snap.Spec
	}
	if len(snap.Labels) > 0 || len(snap.Annotations) > 0 {
		metadata := map[string]any{}
		if len(snap.Labels) > 0 {
			metadata["labels"] = toAnyMap(snap.Labels)
		}
		if len(snap.Annotations) > 0 {
			metadata["annotations"] = toAnyMap(snap.Annotations)
		}
		restorable["metadata"] = metadata
	}
	return restorable
}

// CompileRules turns a policy's rules into the sets BuildPlan consumes, and
// refuses everything that can be refused without a snapshot in hand:
//
//   - Skip rules resolve to known groups or usable paths, and transforms
//     compile, both from NewSkipSet and NewTransformSet.
//   - No path is both skipped and transformed, which contradicts itself.
//   - Every path a person wrote could appear in some snapshot: not denied, not
//     outside the capture allowlist, and not misspelled against ClusterSpec.
//
// The RestorePolicy controller runs this in the background so a bad policy
// shows up on `kubectl get restorepolicy`, and the admission webhook runs it
// again at restore time so the same policy is refused the same way even if its
// status has not caught up. What is left over is genuinely undecidable here —
// whether a valid path is present in one particular history, and what an
// expression produces from a real value — and BuildPlan reports it against the
// snapshot the Cluster actually selects.
func CompileRules(skips []chroniclev1.SkipRule, transforms []chroniclev1.TransformRule) (*SkipSet, *TransformSet, error) {
	skipSet, err := NewSkipSet(skips)
	if err != nil {
		return nil, nil, err
	}
	transformSet, err := NewTransformSet(transforms)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRuleCoherence(skipSet, transformSet); err != nil {
		return nil, nil, err
	}
	if err := validateRulePaths(skips, transforms); err != nil {
		return nil, nil, err
	}
	return skipSet, transformSet, nil
}

// validateRuleCoherence reports a path that is both skipped and transformed.
//
// It needs nothing but the rules: SkipSet already resolves group names to
// paths, so "spec.storage.size is transformed but the Storage group is skipped"
// is answerable from the policy alone. BuildPlan catches the same contradiction
// against a real history, but by then a cluster is being created.
func validateRuleCoherence(skips *SkipSet, transforms *TransformSet) error {
	for _, path := range transforms.Paths() {
		if skipped, reason := skips.Skips(path); skipped {
			return fmt.Errorf(
				"%s is both transformed and skipped (%s); a skipped path is never "+
					"restored, so the transform cannot run", path, reason)
		}
	}
	return nil
}

// validateRulePaths reports a rule naming a path no snapshot could ever hold:
// one on the hard denylist, one no capture group claims, or one that does not
// resolve against CloudNativePG's own types because it is misspelled.
//
// It reads the rules rather than the compiled sets because only the paths a
// person wrote are worth reporting back to them. SkipSet expands a group name
// into that group's paths, and those are kept honest by the coverage tests in
// the snapshot package instead.
func validateRulePaths(
	skips []chroniclev1.SkipRule,
	transforms []chroniclev1.TransformRule,
) error {
	for _, rule := range skips {
		if rule.Path == "" {
			continue
		}
		if err := snapshot.ValidateRulePath(rule.Path); err != nil {
			return fmt.Errorf("skip rule names an unusable path: %w", err)
		}
	}
	for _, rule := range transforms {
		if err := snapshot.ValidateRulePath(rule.Path); err != nil {
			return fmt.Errorf("transform rule names an unusable path: %w", err)
		}
	}
	return nil
}

// BuildPlan resolves a snapshot and a skip set into concrete edits.
//
// Merging happens at leaf granularity rather than by replacing whole objects.
// That is what makes a rule like "skip spec.postgresql.parameters.work_mem"
// meaningful: were the parameters map replaced wholesale, individual GUCs could
// not be carved out of it.
//
// The merge is additive and never deletes. A field the target Cluster sets but
// the snapshot does not know about survives untouched. Restoring cannot
// therefore remove a setting, only add or overwrite one — a deliberately
// conservative choice, since silently deleting something an operator wrote into
// the manifest in front of them would be worse than leaving it.
func BuildPlan(snap *snapshot.Snapshot, skips *SkipSet, transforms *TransformSet) (*Plan, error) {
	if snap == nil {
		return nil, fmt.Errorf("cannot build a plan from a nil snapshot")
	}

	restorable := restorableTree(snap)

	plan := &Plan{}
	fired := map[string]bool{}
	skippedPaths := map[string]string{}
	var leaves []string

	for _, leaf := range pathutil.WalkLeaves(restorable) {
		path := leaf.Path()
		leaves = append(leaves, path)

		// The hard denylist is re-checked here rather than trusted from capture
		// time. A snapshot is an input from an object store, and it may have
		// been written by an older build, or by hand. Restoring
		// .spec.plugins or .spec.backup out of one would point this cluster at
		// the source cluster's WAL archive.
		if snapshot.IsDeniedPath(path) || isDeniedMetadataLeaf(leaf.Segments) {
			plan.Exclusions = append(plan.Exclusions, Exclusion{
				Path: path, Reason: "denied: never restored",
			})
			continue
		}

		if skipped, reason := skips.Skips(path); skipped {
			plan.Exclusions = append(plan.Exclusions, Exclusion{Path: path, Reason: reason})
			skippedPaths[path] = reason
			continue
		}

		value := leaf.Value
		if transformed, applied, err := transforms.Apply(leaf); applied {
			if err != nil {
				return nil, err
			}
			fired[path] = true
			value = transformed
		}

		plan.Changes = append(plan.Changes, Change{
			Path:        path,
			segments:    leaf.Segments,
			value:       value,
			transformed: fired[path],
		})
	}

	if err := transforms.checkAllFired(fired, skippedPaths, leaves); err != nil {
		return nil, err
	}

	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })
	sort.Slice(plan.Exclusions, func(i, j int) bool { return plan.Exclusions[i].Path < plan.Exclusions[j].Path })
	return plan, nil
}

// Apply writes the plan's changes onto a Cluster in its generic JSON form.
func (p *Plan) Apply(cluster map[string]any) error {
	for _, change := range p.Changes {
		if !pathutil.SetSegments(cluster, change.segments, change.value) {
			return fmt.Errorf(
				"cannot restore %s: the target Cluster has a conflicting non-object value on that path",
				change.Path)
		}
	}
	return nil
}

// TransformedPaths returns the paths an expression rewrote, so a restored value
// that does not match the source is traceable.
func (p *Plan) TransformedPaths() []string {
	var out []string
	for _, change := range p.Changes {
		if change.transformed {
			out = append(out, change.Path)
		}
	}
	return out
}

// isDeniedMetadataLeaf reports a label or annotation whose key the capture
// filter refuses. Its segments are used rather than the dotted path because
// the key is the final segment whole, dots and all.
func isDeniedMetadataLeaf(segments []string) bool {
	return len(segments) == 3 &&
		segments[0] == "metadata" &&
		(segments[1] == "labels" || segments[1] == "annotations") &&
		snapshot.IsDeniedMetadataKey(segments[2])
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
