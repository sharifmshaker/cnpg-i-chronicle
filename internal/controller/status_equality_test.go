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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

func withConditions(conditions ...metav1.Condition) *chroniclev1.ConfigStore {
	return &chroniclev1.ConfigStore{
		Status: chroniclev1.ConfigStoreStatus{Conditions: conditions},
	}
}

// The earlier version of this function indexed right.Conditions[0] before
// checking the lengths matched.
func TestEqualStatusHandlesEmptyConditions(t *testing.T) {
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Resolved"}

	if equalStatus(withConditions(ready), withConditions()) {
		t.Error("a store with conditions should not equal one without")
	}
	if equalStatus(withConditions(), withConditions(ready)) {
		t.Error("a store without conditions should not equal one with")
	}
	if !equalStatus(withConditions(), withConditions()) {
		t.Error("two empty statuses should be equal")
	}
}

// Timestamps that move on their own must not count as a change, or the
// controller writes status on every resync and immediately rewakes itself.
func TestEqualStatusIgnoresSelfMovingTimestamps(t *testing.T) {
	earlier := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	later := metav1.NewTime(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	a := withConditions(metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Resolved", LastTransitionTime: earlier,
	})
	a.Status.Resolved = &chroniclev1.ResolvedStore{DestinationPath: "s3://backups/", ObservedAt: &earlier}

	b := withConditions(metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Resolved", LastTransitionTime: later,
	})
	b.Status.Resolved = &chroniclev1.ResolvedStore{DestinationPath: "s3://backups/", ObservedAt: &later}

	if !equalStatus(a, b) {
		t.Error("statuses differing only in timestamps should compare equal")
	}

	b.Status.Resolved.DestinationPath = "s3://other/"
	if equalStatus(a, b) {
		t.Error("a real destination change should compare unequal")
	}
}

// Retention is reported in status, so a change to it must compare unequal or
// the store would keep advertising a window it no longer enforces.
func TestEqualStatusDetectsRetentionChange(t *testing.T) {
	a := withConditions()
	a.Status.Resolved = &chroniclev1.ResolvedStore{Retention: chroniclev1.RetentionForever}
	b := withConditions()
	b.Status.Resolved = &chroniclev1.ResolvedStore{Retention: "30d"}

	if equalStatus(a, b) {
		t.Error("a retention change should compare unequal")
	}
}
