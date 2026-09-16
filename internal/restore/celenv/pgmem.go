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
	"regexp"
	"strconv"
	"strings"
)

// pgMemoryPattern matches a PostgreSQL memory setting such as "128MB". The
// trailing B is optional so that "128M" is read the way a person means it.
var pgMemoryPattern = regexp.MustCompile(`^\s*(-?\d+)\s*([kKmMgGtT][bB]?|[bB])?\s*$`)

// pgMemoryUnits are PostgreSQL's memory unit multipliers, keyed by the
// normalized unit letter ("" for bytes).
//
// PostgreSQL's units are binary — its "MB" is 1024*1024, not 1000*1000. That is
// why these settings cannot be run through Kubernetes quantity parsing, where
// "M" is decimal and only "Mi" is binary. Treating one as the other would shift
// every restored memory setting by about 5%.
var pgMemoryUnits = map[string]int64{
	"":  1,
	"K": 1 << 10,
	"M": 1 << 20,
	"G": 1 << 30,
	"T": 1 << 40,
}

// normalizeUnit reduces any accepted spelling of a unit — "kB", "MB", "m", "B",
// "" — to the single uppercase letter pgMemoryUnits is keyed by.
func normalizeUnit(unit string) (string, bool) {
	normalized := strings.TrimSuffix(strings.ToUpper(unit), "B")
	_, ok := pgMemoryUnits[normalized]
	return normalized, ok
}

// canonicalUnit renders a normalized unit the way PostgreSQL writes it:
// lowercase k, uppercase for the rest, each followed by B; nothing for bytes.
func canonicalUnit(normalized string) string {
	switch normalized {
	case "":
		return ""
	case "K":
		return "kB"
	default:
		return normalized + "B"
	}
}

// ParsePgMemory converts a PostgreSQL memory setting to bytes.
func ParsePgMemory(value string) (int64, error) {
	matches := pgMemoryPattern.FindStringSubmatch(value)
	if matches == nil {
		return 0, fmt.Errorf(
			"%q is not a PostgreSQL memory setting; expected a number with an "+
				"optional kB, MB, GB or TB suffix", value)
	}

	amount, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q has an out-of-range amount: %w", value, err)
	}

	unit, ok := normalizeUnit(matches[2])
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q", value, matches[2])
	}
	return amount * pgMemoryUnits[unit], nil
}

// FormatPgMemory renders bytes as a PostgreSQL memory setting in the given
// unit, which must be one of B, kB, MB, GB or TB.
//
// PostgreSQL rejects a fractional setting, so the result is truncated to whole
// units and an amount that would round to zero is an error rather than a
// silently disabled setting.
func FormatPgMemory(bytes int64, unit string) (string, error) {
	normalized, ok := normalizeUnit(unit)
	if !ok {
		return "", fmt.Errorf("unknown PostgreSQL memory unit %q; use B, kB, MB, GB or TB", unit)
	}

	amount := bytes / pgMemoryUnits[normalized]
	if amount == 0 && bytes != 0 {
		return "", fmt.Errorf(
			"%d bytes rounds to 0%s; PostgreSQL would read that as disabling the setting, "+
				"so choose a smaller unit", bytes, unit)
	}
	return strconv.FormatInt(amount, 10) + canonicalUnit(normalized), nil
}

// UnitOf returns the unit suffix of a PostgreSQL memory setting, so a transform
// can round-trip a value without restating it.
func UnitOf(value string) string {
	matches := pgMemoryPattern.FindStringSubmatch(value)
	if matches == nil {
		return ""
	}
	normalized, _ := normalizeUnit(matches[2])
	return canonicalUnit(normalized)
}
