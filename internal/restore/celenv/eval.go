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
	"fmt"

	"cel.dev/cel-go/cel"
)

// Program is a compiled transform expression, ready to run against a value.
type Program struct {
	source  string
	program cel.Program
}

// Compile checks an expression and prepares it for evaluation.
//
// Compilation is deliberately separable from evaluation so a policy can be
// checked without a snapshot in hand: this catches syntax errors and unknown
// functions the moment a RestorePolicy is applied, rather than at the moment
// someone is trying to create a cluster from it.
func Compile(env *cel.Env, expression string) (*Program, error) {
	if expression == "" {
		return nil, fmt.Errorf("the expression is empty")
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	program, err := env.Program(ast, cel.CostLimit(MaxCost))
	if err != nil {
		return nil, fmt.Errorf("while preparing %q: %w", expression, err)
	}
	return &Program{source: expression, program: program}, nil
}

// Evaluate runs the expression against a captured value and returns a value
// suitable for writing back into a snapshot. quantity says the value's path is
// typed as a Kubernetes quantity; see ToCEL.
func (p *Program) Evaluate(old any, quantity bool) (any, error) {
	bound, err := ToCEL(old, quantity)
	if err != nil {
		return nil, fmt.Errorf("cannot use the captured value in %q: %w", p.source, err)
	}

	out, _, err := p.program.Eval(map[string]any{"old": bound})
	if err != nil {
		return nil, fmt.Errorf("evaluating %q: %w", p.source, err)
	}

	result, err := FromCEL(out, old)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p.source, err)
	}
	return result, nil
}
