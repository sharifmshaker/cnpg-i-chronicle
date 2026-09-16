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

// Package restore turns a captured snapshot plus a restore policy into a set of
// changes to apply to a Cluster being created.
package restore

import (
	"fmt"
	"slices"
	"strings"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
)

// SkipSet decides which paths a restore leaves alone.
type SkipSet struct {
	rules []skipRule
}

// skipRule is one compiled rule. Every rule removes a set of paths; a path is
// skipped when any rule removes it.
type skipRule struct {
	// path is the rule's path with any trailing ".*" removed.
	path string

	// exact is set for a path naming a key inside a map. A map key may contain
	// dots, so nothing beneath it is a separate path to match.
	exact bool

	// keyed is set when the rule narrows a map to some of its keys, by
	// keyPrefix, except, or both.
	keyed     bool
	keyPrefix string
	except    []string

	// reason is how the rule reads in a plan, so a skipped path can be traced
	// to the rule that skipped it.
	reason string
}

// NewSkipSet compiles skip rules.
//
// A path rule matches its own path and everything beneath it. Skipping
// "spec.storage" therefore also skips "spec.storage.size", which is what
// someone writing that rule means.
func NewSkipSet(rules []chroniclev1.SkipRule) (*SkipSet, error) {
	set := &SkipSet{}

	for _, rule := range rules {
		keyed := rule.KeyPrefix != "" || len(rule.Except) > 0

		switch {
		case rule.Group != "" && rule.Path != "":
			return nil, fmt.Errorf(
				"skip rule sets both group %q and path %q; use one or the other",
				rule.Group, rule.Path)

		case rule.Group != "" && keyed:
			return nil, fmt.Errorf(
				"skip rule for group %q sets keyPrefix or except, which apply only to a path naming a map",
				rule.Group)

		case rule.Group != "":
			group, ok := snapshot.LookupGroup(rule.Group)
			if !ok {
				return nil, fmt.Errorf(
					"unknown skip group %q; known groups are %s",
					rule.Group, strings.Join(snapshot.GroupNames(), ", "))
			}
			reason := "group " + group.Name
			for _, path := range group.Paths {
				set.rules = append(set.rules, skipRule{path: path, reason: reason})
			}
			// The Metadata group covers cluster labels and annotations as well
			// as spec.inheritedMetadata, and those live outside the spec.
			if group.Name == snapshot.MetadataGroupName {
				set.rules = append(set.rules,
					skipRule{path: "metadata.labels", reason: reason},
					skipRule{path: "metadata.annotations", reason: reason})
			}

		case rule.Path != "":
			compiled, err := compilePathRule(rule)
			if err != nil {
				return nil, err
			}
			set.rules = append(set.rules, compiled)

		default:
			return nil, fmt.Errorf("skip rule sets neither group nor path")
		}
	}

	return set, nil
}

func compilePathRule(rule chroniclev1.SkipRule) (skipRule, error) {
	path := strings.TrimSuffix(rule.Path, ".*")
	compiled := skipRule{
		path:      path,
		keyed:     rule.KeyPrefix != "" || len(rule.Except) > 0,
		keyPrefix: rule.KeyPrefix,
		except:    rule.Except,
		reason:    "path " + path,
	}

	enclosing, beneath := snapshot.EnclosingMap(path)
	if compiled.keyed {
		if !snapshot.NamesMap(path) {
			return skipRule{}, fmt.Errorf(
				"skip rule on %q sets keyPrefix or except, which apply only to a path naming a "+
					"map, such as metadata.labels or spec.postgresql.parameters", path)
		}
		if rule.KeyPrefix != "" {
			compiled.reason += ", keys prefixed " + rule.KeyPrefix
		}
		if len(rule.Except) > 0 {
			compiled.reason += ", except " + strings.Join(rule.Except, ", ")
		}
	}
	compiled.exact = beneath && path != enclosing
	return compiled, nil
}

// Skips reports whether path is excluded, and why.
func (s *SkipSet) Skips(path string) (bool, string) {
	if s == nil {
		return false, ""
	}
	for _, rule := range s.rules {
		if rule.matches(path) {
			return true, rule.reason
		}
	}
	return false, ""
}

func (r skipRule) matches(path string) bool {
	if r.keyed {
		// A keyed rule names a map, so what follows it in a path is one whole
		// key: a label key or GUC name, dots and all.
		key, beneath := strings.CutPrefix(path, r.path+".")
		if !beneath {
			return false
		}
		return strings.HasPrefix(key, r.keyPrefix) && !slices.Contains(r.except, key)
	}
	if r.exact {
		return path == r.path
	}
	return path == r.path || strings.HasPrefix(path, r.path+".")
}
