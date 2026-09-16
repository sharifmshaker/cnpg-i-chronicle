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

package controller

import (
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

// equalStatus reports whether two stores have the same observable status.
//
// Two fields are excluded because they move on their own: the resolution
// timestamp, and each condition's LastTransitionTime. Comparing them would make
// every resync look like a change, every change trigger a write, and every
// write wake the controller again.
func equalStatus(a, b *chroniclev1.ConfigStore) bool {
	if !equalConditions(a.Status.Conditions, b.Status.Conditions) {
		return false
	}

	left, right := a.Status.Resolved, b.Status.Resolved
	if (left == nil) != (right == nil) {
		return false
	}
	if left == nil {
		return true
	}
	left, right = left.DeepCopy(), right.DeepCopy()
	left.ObservedAt, right.ObservedAt = nil, nil
	return reflect.DeepEqual(left, right)
}

// equalConditions compares two condition lists, ignoring the transition
// timestamps that move on their own.
func equalConditions(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		left.LastTransitionTime, right.LastTransitionTime = metav1.Time{}, metav1.Time{}
		if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}
