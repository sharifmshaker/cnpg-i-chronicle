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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

type stubBackups struct {
	byName map[string]*apiv1.Backup
	byID   map[string]*apiv1.Backup
}

func (s stubBackups) ByName(_ context.Context, _, name string) (*apiv1.Backup, error) {
	if backup, ok := s.byName[name]; ok {
		return backup, nil
	}
	return nil, fmt.Errorf("not found")
}

func (s stubBackups) ByBackupID(_ context.Context, _, id string) (*apiv1.Backup, error) {
	if backup, ok := s.byID[id]; ok {
		return backup, nil
	}
	return nil, fmt.Errorf("not found")
}

func backupEndingAt(name, id string, at *time.Time) *apiv1.Backup {
	backup := &apiv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     apiv1.BackupStatus{BackupID: id},
	}
	if at != nil {
		backup.Status.StoppedAt = &metav1.Time{Time: *at}
	}
	return backup
}

func clusterWith(t *testing.T, bootstrapJSON string) map[string]any {
	t.Helper()
	raw := `{"metadata":{"name":"pg-restored","namespace":"default"},"spec":{"instances":1`
	if bootstrapJSON != "" {
		raw += `,"bootstrap":` + bootstrapJSON
	}
	raw += `}}`
	cluster, err := pathutil.FromJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

var targetMoment = time.Date(2026, 8, 25, 13, 30, 0, 0, time.UTC)

func TestResolveTargetTime(t *testing.T) {
	// CloudNativePG accepts several spellings, and documents that a value with
	// no timezone is UTC. Getting this wrong would select the configuration
	// from the wrong hour.
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"2026-08-25T13:30:00Z", targetMoment},
		{"2026-08-25 13:30:00", targetMoment},
		{"2026-08-25 15:30:00+02", targetMoment},
		{"2026-08-25T13:30:00.000000Z", targetMoment},
	}
	for _, tc := range cases {
		cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"targetTime":"`+tc.raw+`"}}}`)
		got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", nil)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if got.Kind != TargetTime || !got.HasMoment {
			t.Errorf("%q: kind = %v", tc.raw, got.Kind)
			continue
		}
		if !got.Moment.Equal(tc.want) {
			t.Errorf("%q: moment = %v, want %v", tc.raw, got.Moment, tc.want)
		}
	}
}

func TestResolveTargetTimeRejectsGarbage(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"targetTime":"last tuesday"}}}`)
	if _, err := ResolveRecoveryTarget(context.Background(), cluster, "default", nil); err == nil {
		t.Fatal("an unparseable targetTime should be an error, not a silent fallback")
	}
}

func TestResolveBackupID(t *testing.T) {
	backups := stubBackups{byID: map[string]*apiv1.Backup{
		"20260825T133000": backupEndingAt("nightly", "20260825T133000", &targetMoment),
	}}
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"backupID":"20260825T133000"}}}`)

	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetBackupID || !got.Moment.Equal(targetMoment) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveBackupByName(t *testing.T) {
	backups := stubBackups{byName: map[string]*apiv1.Backup{
		"nightly": backupEndingAt("nightly", "20260825T133000", &targetMoment),
	}}
	cluster := clusterWith(t, `{"recovery":{"backup":{"name":"nightly"}}}`)

	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetBackupName || !got.Moment.Equal(targetMoment) {
		t.Errorf("got %+v", got)
	}
}

// A Backup that never finished has no instant to align to.
func TestResolveBackupWithoutStoppedAt(t *testing.T) {
	backups := stubBackups{byName: map[string]*apiv1.Backup{
		"running": backupEndingAt("running", "x", nil),
	}}
	cluster := clusterWith(t, `{"recovery":{"backup":{"name":"running"}}}`)

	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetUnresolvable || got.HasMoment {
		t.Errorf("got %+v, want an unresolvable target", got)
	}
}

func TestResolveMissingBackupIsUnresolvableNotAnError(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"backupID":"nope"}}}`)
	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", stubBackups{})
	if err != nil {
		t.Fatalf("a missing backup should be an unresolvable target, not a hard error: %v", err)
	}
	if got.Kind != TargetUnresolvable {
		t.Errorf("kind = %v, want Unresolvable", got.Kind)
	}
	if !strings.Contains(got.Detail, "nope") {
		t.Errorf("detail does not name the backup: %s", got.Detail)
	}
}

// Targets only PostgreSQL can interpret.
func TestResolveUnresolvableTargets(t *testing.T) {
	for _, target := range []string{
		`{"targetXID":"1234"}`,
		`{"targetLSN":"0/1500000"}`,
		`{"targetName":"before-upgrade"}`,
		`{"targetImmediate":true}`,
	} {
		cluster := clusterWith(t, `{"recovery":{"recoveryTarget":`+target+`}}`)
		got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", stubBackups{})
		if err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		if got.Kind != TargetUnresolvable {
			t.Errorf("%s: kind = %v, want Unresolvable", target, got.Kind)
		}
	}
}

func TestResolveNoRecoveryOrNoTarget(t *testing.T) {
	// A cluster created from scratch.
	got, err := ResolveRecoveryTarget(context.Background(), clusterWith(t, ""), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetNone {
		t.Errorf("kind = %v, want None", got.Kind)
	}

	// Recovery to the end of the archive.
	got, err = ResolveRecoveryTarget(context.Background(),
		clusterWith(t, `{"recovery":{"source":"origin"}}`), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetNone {
		t.Errorf("kind = %v, want None for an untargeted recovery", got.Kind)
	}
}

// --- selection through alignment ---------------------------------------

func alignPolicy(action chroniclev1.UnresolvableAction) *chroniclev1.RestorePolicy {
	return &chroniclev1.RestorePolicy{
		Spec: chroniclev1.RestorePolicySpec{
			Select: chroniclev1.RestoreSelect{
				Mode:           chroniclev1.SelectAlignWithRecoveryTarget,
				OnUnresolvable: action,
			},
		},
	}
}

// The point of the whole mode: recovering data to an earlier moment must bring
// the configuration that was live then, not today's.
func TestSelectAlignedPicksHistoricalConfiguration(t *testing.T) {
	cluster := clusterWith(t,
		`{"recovery":{"recoveryTarget":{"targetTime":"`+
			base.Add(90*time.Minute).Format(time.RFC3339)+`"}}}`)

	got, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(""),
		&Alignment{Cluster: cluster, Namespace: "default", Backups: stubBackups{}})
	if err != nil {
		t.Fatal(err)
	}
	// history() holds captures at +0h, +1h and +2h; 90 minutes in must pick +1h.
	if !got.Snapshot.CapturedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("CapturedAt = %v, want %v", got.Snapshot.CapturedAt, base.Add(time.Hour))
	}
	if got.Target == nil || got.Target.Kind != TargetTime {
		t.Errorf("Target = %+v", got.Target)
	}
}

func TestSelectAlignedUntargetedRecoveryUsesLatest(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"source":"origin"}}`)
	got, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(""),
		&Alignment{Cluster: cluster, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Snapshot.CapturedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("CapturedAt = %v, want the newest", got.Snapshot.CapturedAt)
	}
}

// Fail is the default because the alternative is a cluster whose data and
// configuration silently disagree.
func TestSelectAlignedFailsOnUnresolvableByDefault(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"targetXID":"1234"}}}`)
	_, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(""),
		&Alignment{Cluster: cluster, Namespace: "default", Backups: stubBackups{}})
	if err == nil {
		t.Fatal("an unresolvable target should fail by default")
	}
	for _, want := range []string{"targetXID", "Timestamp", "UseLatest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

func TestSelectAlignedUseLatestOptOut(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"targetLSN":"0/1500000"}}}`)
	got, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(chroniclev1.UnresolvableUseLatest),
		&Alignment{Cluster: cluster, Namespace: "default", Backups: stubBackups{}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Snapshot.CapturedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("CapturedAt = %v, want the newest", got.Snapshot.CapturedAt)
	}
	if got.Target == nil || got.Target.Kind != TargetUnresolvable {
		t.Errorf("the fallback must still record why: %+v", got.Target)
	}
}

// Recovering to a moment before any configuration was captured must fail rather
// than quietly using the oldest one.
func TestSelectAlignedBeforeHistoryFails(t *testing.T) {
	cluster := clusterWith(t,
		`{"recovery":{"recoveryTarget":{"targetTime":"`+
			base.Add(-24*time.Hour).Format(time.RFC3339)+`"}}}`)

	_, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(""),
		&Alignment{Cluster: cluster, Namespace: "default", Backups: stubBackups{}})
	if err == nil {
		t.Fatal("expected a failure when the target predates all captured configuration")
	}
	if !strings.Contains(err.Error(), "does not reach back") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestSelectAlignedNeedsACluster(t *testing.T) {
	if _, err := Select(context.Background(), history(), chroniclev1.SelectAlignWithRecoveryTarget, alignPolicy(""), nil); err == nil {
		t.Fatal("alignment without a cluster should fail explicitly")
	}
}

// CloudNativePG requires backupID alongside targetXID, targetName and
// targetImmediate — it cannot locate a base backup for them either. Checking
// the opaque targets before the backup would report "unresolvable" for every
// cluster CloudNativePG actually accepts.
func TestResolveOpaqueTargetAnchoredToBackup(t *testing.T) {
	backupEnd := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	backups := stubBackups{byID: map[string]*apiv1.Backup{
		"20260825T110000": backupEndingAt("nightly", "20260825T110000", &backupEnd),
	}}

	for _, target := range []string{"targetXID\":\"1234", "targetName\":\"before-upgrade", "targetImmediate\":true"} {
		field := strings.SplitN(target, "\"", 2)[0]
		body := `{"backupID":"20260825T110000","` + target
		if !strings.HasSuffix(target, "true") {
			body += `"`
		}
		body += `}`

		cluster := clusterWith(t, `{"recovery":{"recoveryTarget":`+body+`}}`)
		got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
		if err != nil {
			t.Errorf("%s: %v", field, err)
			continue
		}
		if !got.HasMoment {
			t.Errorf("%s: no moment resolved; detail=%s", field, got.Detail)
			continue
		}
		if got.Kind != TargetApproximate {
			t.Errorf("%s: kind = %v, want Approximate", field, got.Kind)
		}
		if !got.Moment.Equal(backupEnd) {
			t.Errorf("%s: moment = %v, want the backup end %v", field, got.Moment, backupEnd)
		}
		// The approximation must be visible, not silent.
		if !strings.Contains(got.Detail, "closest knowable") {
			t.Errorf("%s: detail does not flag the approximation: %s", field, got.Detail)
		}
	}
}

// backupID on its own is exact, not approximate.
func TestResolveBackupIDAloneIsExact(t *testing.T) {
	backups := stubBackups{byID: map[string]*apiv1.Backup{
		"20260825T133000": backupEndingAt("nightly", "20260825T133000", &targetMoment),
	}}
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"backupID":"20260825T133000"}}}`)

	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetBackupID {
		t.Errorf("kind = %v, want BackupID (exact)", got.Kind)
	}
}

// targetLSN may legitimately appear without a backupID, and then there is
// genuinely nothing to anchor to.
func TestResolveTargetLSNWithoutBackupIsUnresolvable(t *testing.T) {
	cluster := clusterWith(t, `{"recovery":{"recoveryTarget":{"targetLSN":"0/1500000"}}}`)
	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", stubBackups{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetUnresolvable {
		t.Errorf("kind = %v, want Unresolvable", got.Kind)
	}
	if !strings.Contains(got.Detail, "no backupID anchors it") {
		t.Errorf("detail should explain what would fix it: %s", got.Detail)
	}
}

// targetTime still wins over a backupID: it is exact, the backup is not.
func TestResolveTargetTimeBeatsBackupID(t *testing.T) {
	other := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	backups := stubBackups{byID: map[string]*apiv1.Backup{
		"b1": backupEndingAt("b1", "b1", &other),
	}}
	cluster := clusterWith(t,
		`{"recovery":{"recoveryTarget":{"backupID":"b1","targetTime":"2026-08-25T13:30:00Z"}}}`)

	got, err := ResolveRecoveryTarget(context.Background(), cluster, "default", backups)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != TargetTime || !got.Moment.Equal(targetMoment) {
		t.Errorf("got %+v, want the explicit targetTime", got)
	}
}

// The chosen opaque target's name reaches the operator in an admission error.
// Selecting it by ranging over a map made that message vary between runs on an
// identical cluster.
func TestOpaqueTargetSelectionIsDeterministic(t *testing.T) {
	cluster := map[string]any{"spec": map[string]any{"bootstrap": map[string]any{
		"recovery": map[string]any{"recoveryTarget": map[string]any{
			"targetLSN":       "0/16B3748",
			"targetName":      "checkpoint",
			"targetImmediate": true,
		}},
	}}}

	first := opaqueTargetIn(cluster)
	for i := range 200 {
		if got := opaqueTargetIn(cluster); got != first {
			t.Fatalf("iteration %d returned %q, but the first returned %q", i, got, first)
		}
	}
	if first != "targetLSN" {
		t.Errorf("opaqueTargetIn = %q; want targetLSN, the first in declared order", first)
	}
	if opaqueTargetIn(map[string]any{}) != "" {
		t.Error("a cluster with no opaque target should report none")
	}
}
