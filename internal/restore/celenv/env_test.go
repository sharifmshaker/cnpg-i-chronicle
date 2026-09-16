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
	"strings"
	"testing"
)

// eval runs an expression against a value on an ordinary path, and
// evalQuantity against one on a path CloudNativePG types as a quantity.
func eval(t *testing.T, expression string, old any) (any, error) {
	t.Helper()
	return evaluate(t, expression, old, false)
}

func evalQuantity(t *testing.T, expression string, old any) (any, error) {
	t.Helper()
	return evaluate(t, expression, old, true)
}

func evaluate(t *testing.T, expression string, old any, quantity bool) (any, error) {
	t.Helper()
	env, err := New()
	if err != nil {
		t.Fatal(err)
	}
	program, err := Compile(env, expression)
	if err != nil {
		return nil, err
	}
	return program.Evaluate(old, quantity)
}

// Arithmetic on a size, written as a member call.
//
// `old * 5` would read better, but cel-go binds arithmetic operators as
// singletons in its standard library and rejects specialized overloads on them,
// so a custom quantity type cannot participate in `*`. This is the same shape
// Kubernetes' own quantity CEL library settled on.
func TestQuantityArithmeticReadsNaturally(t *testing.T) {
	cases := []struct {
		expression string
		old        any
		want       string
	}{
		{"old.multiply(5)", "10Gi", "50Gi"},
		{"old.multiply(2)", "2Gi", "4Gi"},
		{"old.divide(2)", "10Gi", "5Gi"},
		{"old.multiply(1.5)", "2Gi", "3Gi"},
		{"old.add(quantity('1Gi'))", "2Gi", "3Gi"},
		{"old.sub(quantity('512Mi'))", "2Gi", "1536Mi"},
		{"old.multiply(2)", "500m", "1"},
		{"old.divide(2)", "500m", "250m"},

		// Binary stays binary and decimal stays decimal; a restored 10Gi volume
		// should not come back as 53687091200.
		{"old.multiply(5)", "10G", "50G"},
	}

	for _, tc := range cases {
		got, err := evalQuantity(t, tc.expression, tc.old)
		if err != nil {
			t.Errorf("%s on %v: %v", tc.expression, tc.old, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s on %v = %v, want %v", tc.expression, tc.old, got, tc.want)
		}
	}
}

// Plain integers need no wrapper at all.
func TestIntegerArithmetic(t *testing.T) {
	got, err := eval(t, "old * 2", json.Number("3"))
	if err != nil {
		t.Fatal(err)
	}
	if got != json.Number("6") {
		t.Errorf("got %v, want 6", got)
	}

	// Clamping, which is the other thing people actually want.
	got, err = eval(t, "old > 3 ? 3 : old", json.Number("9"))
	if err != nil {
		t.Fatal(err)
	}
	if got != json.Number("3") {
		t.Errorf("got %v, want 3", got)
	}
}

// A floating-point result must render as a plain decimal. %g would give 1e+06
// for a million, which is a valid JSON number but not a valid integer field.
func TestDoubleResultsRenderAsPlainDecimals(t *testing.T) {
	got, err := eval(t, "double(old) * 1000000.0", json.Number("1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != json.Number("1000000") {
		t.Errorf("got %v, want 1000000", got)
	}
	got, err = eval(t, "double(old) / 4.0", json.Number("1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != json.Number("0.25") {
		t.Errorf("got %v, want 0.25", got)
	}
}

// PostgreSQL's MB is binary; Kubernetes' M is decimal. Conflating them would
// shift every restored memory setting by about 5%.
func TestPostgresMemoryIsNotKubernetesQuantity(t *testing.T) {
	bound, err := ToCEL("128MB", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, isString := bound.(string); !isString {
		t.Fatalf("a GUC's 128MB must reach an expression as a string, got %T", bound)
	}

	bytes, err := ParsePgMemory("128MB")
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 128*1024*1024 {
		t.Errorf("128MB = %d bytes, want %d (binary, not decimal)", bytes, 128*1024*1024)
	}
}

func TestPgMemoryTransforms(t *testing.T) {
	cases := []struct {
		expression string
		old        string
		want       string
	}{
		{`pgScale(old, 2)`, "128MB", "256MB"},
		{`pgScale(old, 0.5)`, "128MB", "64MB"},
		{`pgScale(old, 2)`, "64kB", "128kB"},
		{`pgScale(old, 4)`, "1GB", "4GB"},
		{`pgMemFormat(pgMem(old) * 2, "MB")`, "128MB", "256MB"},
		{`pgMemFormat(pgMem(old), "kB")`, "1MB", "1024kB"},
	}
	for _, tc := range cases {
		got, err := eval(t, tc.expression, tc.old)
		if err != nil {
			t.Errorf("%s on %s: %v", tc.expression, tc.old, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s on %s = %v, want %v", tc.expression, tc.old, got, tc.want)
		}
	}
}

// PostgreSQL reads a fractional setting as zero, which silently disables it.
func TestPgMemoryRefusesToRoundToZero(t *testing.T) {
	_, err := eval(t, `pgMemFormat(pgMem(old), "GB")`, "64MB")
	if err == nil {
		t.Fatal("expected an error rather than 0GB")
	}
	if !strings.Contains(err.Error(), "rounds to 0") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestCompileRejectsBadExpressions(t *testing.T) {
	env, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{
		"",
		"old *",
		"noSuchFunction(old)",
		"old.bogusMethod()",
	} {
		if _, err := Compile(env, expression); err == nil {
			t.Errorf("Compile(%q) succeeded; want an error", expression)
		}
	}
}

// A type mismatch cannot be caught at compile time because `old` is dyn. It
// must fail clearly at evaluation instead of producing a wrong value.
func TestTypeMismatchFailsAtEvaluation(t *testing.T) {
	_, err := eval(t, "old.multiply(2)", "on")
	if err == nil {
		t.Fatal("multiplying a non-numeric GUC should fail")
	}
}

func TestDivideByZero(t *testing.T) {
	if _, err := evalQuantity(t, "old.divide(0)", "10Gi"); err == nil {
		t.Fatal("dividing a quantity by zero should fail")
	}
}

// Transforms apply to one scalar. A list or object has no meaningful
// arithmetic, and should say so rather than misbehave.
func TestNonScalarIsRejected(t *testing.T) {
	_, err := eval(t, "old", []any{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "not to lists or objects") {
		t.Errorf("err = %v; want a clear rejection of non-scalars", err)
	}
}

// How `old` is typed is decided by the path, and results go back in the JSON
// type the captured value had. Each row is a case that went wrong when the type
// was guessed from the value instead.
func TestValuesAreTypedByPathAndWrittenBackInTheirOwnType(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expression string
		old        any
		quantity   bool
		want       any
		wantErr    string
	}{
		// GUCs are strings holding numbers: arithmetic works, and the result
		// is a string again, because parameters admits nothing else.
		{name: "integer GUC", expression: "old * 2", old: "200", want: "400"},
		{name: "integer GUC clamped", expression: "old > 100 ? 100 : old", old: "200", want: "100"},
		{name: "decimal GUC untouched", expression: "old", old: "1.1", want: "1.1"},
		{name: "decimal GUC scaled", expression: "old * 2.0", old: "1.1", want: "2.2"},
		{name: "boolean result on a GUC", expression: "old > 100", old: "200", want: "true"},
		{name: "memory GUC", expression: "pgScale(old, 2)", old: "16MB", want: "32MB"},
		{name: "word GUC", expression: "old", old: "on", want: "on"},
		{name: "too large for an int stays text", expression: "old", old: "99999999999999999999", want: "99999999999999999999"},
		{name: "not a number just because strconv says so", expression: "old", old: "inf", want: "inf"},

		// Quantity paths: "2" and "500m" are both quantities, so one
		// expression works on either.
		{name: "whole CPU", expression: "old.multiply(2)", old: "2", quantity: true, want: "4"},
		{name: "milli CPU", expression: "old.multiply(2)", old: "500m", quantity: true, want: "1"},
		{name: "storage", expression: "old.divide(10)", old: "10Gi", quantity: true, want: "1Gi"},

		// Integer fields stay integers.
		{name: "instances", expression: "old > 1 ? 1 : old", old: json.Number("3"), want: json.Number("1")},

		// The one thing that gets stricter: a numeric GUC is a number, not a
		// quantity.
		{name: "quantity method on a GUC", expression: "old.multiply(2)", old: "200", wantErr: "no such overload"},
		{name: "non-quantity on a quantity path", expression: "old", old: "lots", quantity: true, wantErr: "not a Kubernetes quantity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluate(t, tc.expression, tc.old, tc.quantity)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %#v, %v; want an error mentioning %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
