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
	"reflect"
	"testing"

	"cel.dev/cel-go/common/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

func mustQuantity(t *testing.T, s string) Quantity {
	t.Helper()
	q, err := ParseQuantity(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// Quantity implements ref.Val, so CEL can convert it. Getting these wrong
// surfaces as a baffling type error deep inside an expression.
func TestQuantityConvertToType(t *testing.T) {
	q := mustQuantity(t, "2Gi")

	if got := q.ConvertToType(types.StringType); got.Value() != "2Gi" {
		t.Errorf("string conversion = %v", got.Value())
	}
	if got := q.ConvertToType(types.IntType); got.Value() != int64(2147483648) {
		t.Errorf("int conversion = %v", got.Value())
	}
	if got := q.ConvertToType(QuantityType); got.Type() != QuantityType {
		t.Errorf("identity conversion = %v", got.Type())
	}
	if got := q.ConvertToType(types.TypeType); got.Value() != QuantityType.TypeName() {
		t.Errorf("type conversion = %v, want %v", got.Value(), QuantityType.TypeName())
	}
	if got := q.ConvertToType(types.BoolType); !types.IsError(got) {
		t.Errorf("a nonsense conversion should be an error, got %v", got)
	}
}

func TestQuantityConvertToNative(t *testing.T) {
	q := mustQuantity(t, "500m")

	asString, err := q.ConvertToNative(reflect.TypeOf(""))
	if err != nil || asString != "500m" {
		t.Errorf("string = %v, %v", asString, err)
	}
	asInt, err := q.ConvertToNative(reflect.TypeOf(int64(0)))
	if err != nil || asInt != int64(1) {
		t.Errorf("int = %v, %v", asInt, err)
	}
	if _, err := q.ConvertToNative(reflect.TypeOf(&resource.Quantity{})); err != nil {
		t.Errorf("quantity pointer = %v", err)
	}
	if _, err := q.ConvertToNative(reflect.TypeOf(struct{}{})); err == nil {
		t.Error("an unsupported native type should error")
	}
}

func TestQuantityEqualAndValue(t *testing.T) {
	twoGi := mustQuantity(t, "2Gi")

	// 2048Mi and 2Gi are the same amount written differently.
	if got := twoGi.Equal(mustQuantity(t, "2048Mi")); got != types.True {
		t.Errorf("2Gi == 2048Mi returned %v", got)
	}
	if got := twoGi.Equal(mustQuantity(t, "1Gi")); got != types.False {
		t.Errorf("2Gi == 1Gi returned %v", got)
	}
	if got := twoGi.Equal(types.String("2Gi")); !types.IsError(got) {
		t.Errorf("comparing against a string should error, got %v", got)
	}
	if twoGi.Value() == nil || twoGi.Type() != QuantityType {
		t.Error("Value/Type are not wired up")
	}
	if NewQuantity(twoGi.Q).Q.String() != "2Gi" {
		t.Error("NewQuantity did not wrap the quantity")
	}
}

func TestParseQuantityRejectsNonsense(t *testing.T) {
	for _, s := range []string{"", "on", "128MB", "five gigabytes"} {
		if _, err := ParseQuantity(s); err == nil {
			t.Errorf("ParseQuantity(%q) should fail", s)
		}
	}
}

// Scaling must not silently truncate a small CPU quantity to zero, which would
// restore a cluster with no CPU request at all.
func TestScalePreservesSmallQuantities(t *testing.T) {
	got, err := evalQuantity(t, "old.divide(4)", "500m")
	if err != nil {
		t.Fatal(err)
	}
	if got != "125m" {
		t.Errorf("500m / 4 = %v, want 125m", got)
	}
}

func TestScaleRejectsNonsenseFactors(t *testing.T) {
	if _, err := evalQuantity(t, "old.multiply(0.0/0.0)", "1Gi"); err == nil {
		t.Error("scaling by NaN should fail")
	}
}
