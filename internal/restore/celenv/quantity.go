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

// Package celenv builds the CEL environment restore transforms are evaluated in.
package celenv

import (
	"fmt"
	"math"
	"reflect"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/api/resource"
)

// QuantityType is the CEL type for a Kubernetes resource quantity.
var QuantityType = types.NewOpaqueType("kubernetes.Quantity")

// Quantity wraps a resource.Quantity as a CEL value.
//
// It exists so that a transform can be written as `old.multiply(5)` and get
// back `50Gi` rather than a byte count. Kubernetes' own CEL quantity library
// has no multiply or divide — only add, sub and comparisons — so the arithmetic
// that matters most for resizing a restored cluster is not expressible with it.
type Quantity struct {
	Q *resource.Quantity
}

// NewQuantity wraps a parsed quantity.
func NewQuantity(q *resource.Quantity) Quantity { return Quantity{Q: q} }

// ConvertToNative implements ref.Val.
func (q Quantity) ConvertToNative(typeDesc reflect.Type) (any, error) {
	switch typeDesc.Kind() {
	case reflect.String:
		return q.Q.String(), nil
	case reflect.Int64, reflect.Int:
		return q.Q.Value(), nil
	case reflect.Float64:
		return q.Q.AsApproximateFloat64(), nil
	}
	if typeDesc == reflect.TypeOf(&resource.Quantity{}) {
		return q.Q, nil
	}
	return nil, fmt.Errorf("cannot convert a quantity to %v", typeDesc)
}

// ConvertToType implements ref.Val.
func (q Quantity) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case QuantityType:
		return q
	case types.StringType:
		return types.String(q.Q.String())
	case types.IntType:
		return types.Int(q.Q.Value())
	case types.DoubleType:
		return types.Double(q.Q.AsApproximateFloat64())
	case types.TypeType:
		return QuantityType
	}
	return types.NewErr("cannot convert a quantity to %v", typeVal)
}

// Equal implements ref.Val.
func (q Quantity) Equal(other ref.Val) ref.Val {
	otherQuantity, ok := other.(Quantity)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(q.Q.Cmp(*otherQuantity.Q) == 0)
}

// Type implements ref.Val.
func (q Quantity) Type() ref.Type { return QuantityType }

// Value implements ref.Val.
func (q Quantity) Value() any { return q.Q }

// scale multiplies a quantity by a factor, preserving its format so that a
// binary size stays binary: 10Gi times 5 is 50Gi, not 53687091200.
func scale(q *resource.Quantity, factor float64) (Quantity, error) {
	if math.IsNaN(factor) || math.IsInf(factor, 0) {
		return Quantity{}, fmt.Errorf("cannot scale a quantity by %v", factor)
	}

	// Work in milli-units so that a fractional factor on a small quantity, such
	// as halving 500m of CPU, does not truncate to zero.
	scaled := float64(q.MilliValue()) * factor
	if scaled > math.MaxInt64 || scaled < math.MinInt64 {
		return Quantity{}, fmt.Errorf("scaling %s by %v overflows", q.String(), factor)
	}

	result := resource.NewMilliQuantity(int64(scaled), q.Format)
	return Quantity{Q: result}, nil
}

// ParseQuantity turns a string into a CEL quantity value.
func ParseQuantity(s string) (Quantity, error) {
	parsed, err := resource.ParseQuantity(s)
	if err != nil {
		return Quantity{}, fmt.Errorf("%q is not a Kubernetes quantity: %w", s, err)
	}
	return Quantity{Q: &parsed}, nil
}
