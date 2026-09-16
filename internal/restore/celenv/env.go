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
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// MaxCost bounds evaluation. It is a backstop rather than the active control,
// and it is worth being precise about which is which.
//
// `old` binds to a single captured value and this environment declares no
// comprehensions, so there is no way to iterate: evaluation cost stays linear
// in the size of the expression. What actually bounds the work is the 2048
// character limit on RestorePolicy's expression field, enforced by the API
// server before admission is reached, and cel-go's own parser limits — an
// expression pathological enough to matter fails to compile with "max recursion
// depth exceeded" long before this budget is approached.
//
// It stays because that reasoning depends on the environment staying free of
// comprehensions. Declare a list function here and this becomes load-bearing.
const MaxCost = 1_000_000

// New builds the CEL environment transforms are compiled and evaluated in.
//
// `old` is declared as dyn because its type depends on the captured value, not
// on the expression: an integer for spec.instances, a quantity for
// spec.storage.size, a plain string for a GUC PostgreSQL spells its own way.
// Compilation therefore catches syntax errors and unknown functions, while a
// type mismatch can only surface on evaluation, against the real value — which
// is why a restore fails rather than silently producing a wrong one.
func New() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("old", cel.DynType),

		// A quantity literal, so an expression can add or compare against a
		// size it names itself: old.add(quantity('1Gi')).
		cel.Function("quantity",
			cel.Overload("quantity_string", []*cel.Type{cel.StringType}, QuantityType,
				cel.UnaryBinding(func(value ref.Val) ref.Val {
					text, ok := value.(types.String)
					if !ok {
						return types.MaybeNoSuchOverloadErr(value)
					}
					quantity, err := ParseQuantity(string(text))
					if err != nil {
						return types.NewErr("%s", err.Error())
					}
					return quantity
				}))),

		// Member functions rather than the * and / operators.
		//
		// cel-go binds arithmetic operators in its standard library as
		// singletons, and refuses to mix a singleton with a specialized
		// overload ("singleton function incompatible with specialized
		// overloads"), so `old * 5` on a quantity cannot be made to work. This
		// is the same shape Kubernetes' own quantity CEL library uses. Plain
		// integers, such as spec.instances, still take the operators directly.
		cel.Function("multiply",
			quantityScaleOverload("quantity_multiply_int", cel.IntType, false),
			quantityScaleOverload("quantity_multiply_double", cel.DoubleType, false)),

		cel.Function("divide",
			quantityScaleOverload("quantity_divide_int", cel.IntType, true),
			quantityScaleOverload("quantity_divide_double", cel.DoubleType, true)),

		cel.Function("add",
			cel.MemberOverload("quantity_add_quantity", []*cel.Type{QuantityType, QuantityType}, QuantityType,
				cel.BinaryBinding(quantityCombine(false)))),
		cel.Function("sub",
			cel.MemberOverload("quantity_sub_quantity", []*cel.Type{QuantityType, QuantityType}, QuantityType,
				cel.BinaryBinding(quantityCombine(true)))),

		cel.Function("bytes",
			cel.MemberOverload("quantity_bytes", []*cel.Type{QuantityType}, cel.IntType,
				cel.UnaryBinding(func(value ref.Val) ref.Val {
					quantity, ok := value.(Quantity)
					if !ok {
						return types.MaybeNoSuchOverloadErr(value)
					}
					return types.Int(quantity.Q.Value())
				}))),

		// PostgreSQL memory settings. Separate from quantities because
		// PostgreSQL's MB is binary while Kubernetes' M is decimal.
		cel.Function("pgMem",
			cel.Overload("pgmem_string", []*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(func(value ref.Val) ref.Val {
					text, ok := value.(types.String)
					if !ok {
						return types.MaybeNoSuchOverloadErr(value)
					}
					parsed, err := ParsePgMemory(string(text))
					if err != nil {
						return types.NewErr("%s", err.Error())
					}
					return types.Int(parsed)
				}))),

		cel.Function("pgMemFormat",
			cel.Overload("pgmemformat_int_string", []*cel.Type{cel.IntType, cel.StringType}, cel.StringType,
				cel.BinaryBinding(func(amount, unit ref.Val) ref.Val {
					count, ok := amount.(types.Int)
					if !ok {
						return types.MaybeNoSuchOverloadErr(amount)
					}
					suffix, ok := unit.(types.String)
					if !ok {
						return types.MaybeNoSuchOverloadErr(unit)
					}
					formatted, err := FormatPgMemory(int64(count), string(suffix))
					if err != nil {
						return types.NewErr("%s", err.Error())
					}
					return types.String(formatted)
				}))),

		// pgScale is the shorthand the common case deserves: double a memory
		// GUC without naming its unit twice.
		cel.Function("pgScale",
			cel.Overload("pgscale_string_double", []*cel.Type{cel.StringType, cel.DoubleType}, cel.StringType,
				cel.BinaryBinding(pgScale)),
			cel.Overload("pgscale_string_int", []*cel.Type{cel.StringType, cel.IntType}, cel.StringType,
				cel.BinaryBinding(pgScale))),
	)
}

// quantityScaleOverload builds a multiply or divide overload for quantities.
func quantityScaleOverload(name string, operand *cel.Type, divide bool) cel.FunctionOpt {
	return cel.MemberOverload(name, []*cel.Type{QuantityType, operand}, QuantityType,
		cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
			quantity, ok := lhs.(Quantity)
			if !ok {
				return types.MaybeNoSuchOverloadErr(lhs)
			}

			factor, err := asFloat(rhs)
			if err != nil {
				return types.NewErr("%s", err.Error())
			}
			if divide {
				if factor == 0 {
					return types.NewErr("cannot divide %s by zero", quantity.Q.String())
				}
				factor = 1 / factor
			}

			scaled, err := scale(quantity.Q, factor)
			if err != nil {
				return types.NewErr("%s", err.Error())
			}
			return scaled
		}))
}

func quantityCombine(subtract bool) func(lhs, rhs ref.Val) ref.Val {
	return func(lhs, rhs ref.Val) ref.Val {
		left, ok := lhs.(Quantity)
		if !ok {
			return types.MaybeNoSuchOverloadErr(lhs)
		}
		right, ok := rhs.(Quantity)
		if !ok {
			return types.MaybeNoSuchOverloadErr(rhs)
		}

		result := left.Q.DeepCopy()
		if subtract {
			result.Sub(*right.Q)
		} else {
			result.Add(*right.Q)
		}
		return Quantity{Q: &result}
	}
}

func pgScale(value, factor ref.Val) ref.Val {
	text, ok := value.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(value)
	}
	multiplier, err := asFloat(factor)
	if err != nil {
		return types.NewErr("%s", err.Error())
	}

	parsed, err := ParsePgMemory(string(text))
	if err != nil {
		return types.NewErr("%s", err.Error())
	}
	unit := UnitOf(string(text))

	formatted, err := FormatPgMemory(int64(float64(parsed)*multiplier), unit)
	if err != nil {
		return types.NewErr("%s", err.Error())
	}
	return types.String(formatted)
}

func asFloat(value ref.Val) (float64, error) {
	switch typed := value.(type) {
	case types.Int:
		return float64(typed), nil
	case types.Double:
		return float64(typed), nil
	case types.Uint:
		return float64(typed), nil
	}
	return 0, fmt.Errorf("expected a number, got %v", value.Type())
}

// plainInteger and plainDecimal recognise a string that is a number and
// nothing else. strconv alone is too permissive here: it also reads "inf",
// "NaN" and hexadecimal floats, none of which a GUC means as a number.
var (
	plainInteger = regexp.MustCompile(`^[+-]?\d+$`)
	plainDecimal = regexp.MustCompile(`^[+-]?(\d+\.\d*|\.\d+|\d+)([eE][+-]?\d+)?$`)
)

// ToCEL converts a captured JSON value into the CEL value bound to `old`.
//
// quantity is decided by the path, not by the value: the caller passes true
// when CloudNativePG types the field as a resource quantity. On such a path
// every value is a quantity, "2" as much as "500m", so one expression behaves
// the same against every cluster's history.
//
// Everywhere else a string that is a plain number is bound as an int or a
// double. That is what lets `old * 2` work on max_connections, which
// CloudNativePG carries as the string "200", and what keeps "1.1" from being
// read as a quantity and written back as "1100m". Any other string stays a
// string, so PostgreSQL's own memory spelling such as "128MB" is never mistaken
// for Kubernetes syntax.
func ToCEL(value any, quantity bool) (any, error) {
	if value == nil {
		return nil, fmt.Errorf("the captured value is null")
	}

	if quantity {
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case json.Number:
			text = typed.String()
		default:
			return nil, fmt.Errorf("the captured value %v should be a quantity but is a %T", value, value)
		}
		return ParseQuantity(text)
	}

	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
		return typed.Float64()
	case string:
		if plainInteger.MatchString(typed) {
			if integer, err := strconv.ParseInt(typed, 10, 64); err == nil {
				return integer, nil
			}
			// Too large for an int64. Converting it to a double would quietly
			// lose digits, so it stays text.
			return typed, nil
		}
		if plainDecimal.MatchString(typed) {
			if decimal, err := strconv.ParseFloat(typed, 64); err == nil {
				return decimal, nil
			}
		}
		return typed, nil
	case bool, int64, float64:
		return typed, nil
	}
	return nil, fmt.Errorf(
		"values of type %T cannot be transformed; transforms apply to scalars, not to lists or objects",
		value)
}

// FromCEL converts an expression result back into a value for the snapshot,
// in the JSON type the captured value had.
//
// That matters because CloudNativePG's types are strict about it. GUCs are a
// map of strings, so a transform on max_connections that returns the int 400
// must be written as "400": written as a number, the API server rejects the
// whole Cluster. The reverse holds too — spec.instances is an integer and
// stays one.
func FromCEL(value ref.Val, source any) (any, error) {
	if _, sourceIsString := source.(string); sourceIsString {
		return formatAsString(value)
	}

	switch typed := value.(type) {
	case Quantity:
		return typed.Q.String(), nil
	case types.String:
		return string(typed), nil
	case types.Int:
		return json.Number(strconv.FormatInt(int64(typed), 10)), nil
	case types.Uint:
		return json.Number(strconv.FormatUint(uint64(typed), 10)), nil
	case types.Double:
		return json.Number(formatDouble(float64(typed))), nil
	case types.Bool:
		return bool(typed), nil
	}
	return nil, fmt.Errorf("a transform produced an unsupported result of type %v", value.Type())
}

// formatAsString renders a result for a field held as a string.
func formatAsString(value ref.Val) (string, error) {
	switch typed := value.(type) {
	case Quantity:
		return typed.Q.String(), nil
	case types.String:
		return string(typed), nil
	case types.Int:
		return strconv.FormatInt(int64(typed), 10), nil
	case types.Uint:
		return strconv.FormatUint(uint64(typed), 10), nil
	case types.Double:
		return formatDouble(float64(typed)), nil
	case types.Bool:
		return strconv.FormatBool(bool(typed)), nil
	}
	return "", fmt.Errorf("a transform produced an unsupported result of type %v", value.Type())
}

// formatDouble uses 'f' rather than %g: a large result must not come back as
// 1e+06, which is a valid JSON number but neither a valid integer field nor
// how PostgreSQL expects a setting written.
func formatDouble(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
