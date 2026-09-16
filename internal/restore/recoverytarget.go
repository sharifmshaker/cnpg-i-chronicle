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
	"time"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	machinerytypes "github.com/cloudnative-pg/machinery/pkg/types"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

// TargetKind is how a Cluster's recovery target was interpreted.
type TargetKind string

const (
	// TargetNone means the cluster recovers to the end of the archive, so the
	// most recent configuration is the right one.
	TargetNone TargetKind = "None"

	// TargetTime means recoveryTarget.targetTime named an instant directly.
	TargetTime TargetKind = "TargetTime"

	// TargetBackupName means bootstrap.recovery.backup named a Backup resource.
	TargetBackupName TargetKind = "BackupName"

	// TargetBackupID means recoveryTarget.backupID named a backup, resolved
	// through the Backup resource that recorded it.
	TargetBackupID TargetKind = "BackupID"

	// TargetApproximate means the target is anchored to a base backup but
	// replays WAL beyond it, so the moment is a lower bound rather than exact.
	TargetApproximate TargetKind = "Approximate"

	// TargetUnresolvable means a target was set that cannot be mapped to a
	// moment without replaying WAL, and no base backup anchors it.
	TargetUnresolvable TargetKind = "Unresolvable"
)

// TargetResolution is a recovery target reduced to a moment, where possible.
type TargetResolution struct {
	Kind TargetKind

	// Moment is the instant the restored data will correspond to. Only
	// meaningful when HasMoment is true.
	Moment time.Time

	// HasMoment is false for TargetNone and TargetUnresolvable.
	HasMoment bool

	// Detail explains the resolution, for conditions and error messages.
	Detail string
}

// BackupLookup finds the Backup resources that recorded a physical backup.
type BackupLookup interface {
	// ByName returns the Backup with this name in the namespace.
	ByName(ctx context.Context, namespace, name string) (*apiv1.Backup, error)

	// ByBackupID returns the Backup whose status recorded this backup id.
	ByBackupID(ctx context.Context, namespace, backupID string) (*apiv1.Backup, error)
}

// opaqueTargets names the recovery targets only PostgreSQL can interpret, in a
// fixed order. It is a slice rather than a map because the chosen name reaches
// the operator in an admission error message, and ranging over a map would make
// that message differ between runs on the same cluster.
var opaqueTargets = []string{"targetXID", "targetLSN", "targetName", "targetImmediate"}

// opaqueTargetIn returns the first opaque target the cluster sets, or "".
func opaqueTargetIn(cluster map[string]any) string {
	for _, name := range opaqueTargets {
		if _, present := pathutil.Get(cluster, "spec.bootstrap.recovery.recoveryTarget."+name); present {
			return name
		}
	}
	return ""
}

// ResolveRecoveryTarget reads a Cluster's bootstrap stanza and works out which
// moment in the source cluster's life its data will correspond to.
//
// The whole point of aligning to it: restoring a database to last Tuesday and
// then applying today's configuration gives a cluster that never existed. The
// configuration that was in force at the recovery target is the one that
// matches the data being recovered.
func ResolveRecoveryTarget(
	ctx context.Context,
	cluster map[string]any,
	namespace string,
	backups BackupLookup,
) (*TargetResolution, error) {
	recovery, found := pathutil.Get(cluster, "spec.bootstrap.recovery")
	if !found {
		return &TargetResolution{
			Kind:   TargetNone,
			Detail: "the cluster is not bootstrapping from a recovery, so the latest configuration applies",
		}, nil
	}
	if _, ok := recovery.(map[string]any); !ok {
		return &TargetResolution{Kind: TargetNone, Detail: "no recovery configuration"}, nil
	}

	// An explicit instant needs no lookup. Parsing goes through the same helper
	// CloudNativePG's own webhook uses, so a value it accepts is interpreted
	// identically here — a divergence would silently select a different
	// configuration than the data being recovered.
	if raw, ok := stringAt(cluster, "spec.bootstrap.recovery.recoveryTarget.targetTime"); ok {
		moment, err := machinerytypes.ParseTargetTime(nil, raw)
		if err != nil {
			return nil, fmt.Errorf("cannot parse recoveryTarget.targetTime %q: %w", raw, err)
		}
		return &TargetResolution{
			Kind: TargetTime, Moment: moment.UTC(), HasMoment: true,
			Detail: fmt.Sprintf("recoveryTarget.targetTime %s", moment.UTC().Format(time.RFC3339)),
		}, nil
	}

	// Targets only PostgreSQL can interpret. A transaction id or an LSN says
	// nothing about wall-clock time without replaying the WAL, which is not
	// something an admission webhook can do.
	//
	// CloudNativePG requires backupID alongside targetXID, targetName and
	// targetImmediate, precisely because it cannot locate a base backup for
	// them either. So the backup is checked first: when it is present the
	// recovery is anchored to it, and the configuration at the end of that
	// backup is the closest knowable match. Checking the opaque targets first
	// would report "unresolvable" for every cluster CloudNativePG actually
	// accepts, which is to say all of them.
	opaque := opaqueTargetIn(cluster)

	if backupID, ok := stringAt(cluster, "spec.bootstrap.recovery.recoveryTarget.backupID"); ok {
		if backups == nil {
			return &TargetResolution{
				Kind:   TargetUnresolvable,
				Detail: fmt.Sprintf("recoveryTarget.backupID %q cannot be resolved here", backupID),
			}, nil
		}
		backup, err := backups.ByBackupID(ctx, namespace, backupID)
		if err != nil {
			return &TargetResolution{
				Kind: TargetUnresolvable,
				Detail: fmt.Sprintf(
					"no Backup resource in namespace %q records backupID %q, so the moment it "+
						"corresponds to is unknown (%v)", namespace, backupID, err),
			}, nil
		}

		kind, detail := TargetBackupID, fmt.Sprintf("recoveryTarget.backupID %s", backupID)
		if opaque != "" {
			// Recovery replays WAL past the backup to reach the target, so the
			// backup's end is a lower bound on the real recovery point.
			kind = TargetApproximate
			detail = fmt.Sprintf(
				"recoveryTarget.%s anchored to backupID %s; the configuration is taken from "+
					"the end of that backup, which is the closest knowable point",
				opaque, backupID)
		}
		return resolutionFromBackup(kind, backup, detail)
	}

	if opaque != "" {
		return &TargetResolution{
			Kind: TargetUnresolvable,
			Detail: fmt.Sprintf(
				"recoveryTarget.%s cannot be mapped to a point in time without replaying WAL, "+
					"and no backupID anchors it to a base backup", opaque),
		}, nil
	}

	// Recovering from a named Backup with no explicit target restores to the
	// end of that backup.
	if name, ok := stringAt(cluster, "spec.bootstrap.recovery.backup.name"); ok {
		if backups == nil {
			return &TargetResolution{
				Kind:   TargetUnresolvable,
				Detail: fmt.Sprintf("Backup %q cannot be resolved here", name),
			}, nil
		}
		backup, err := backups.ByName(ctx, namespace, name)
		if err != nil {
			return &TargetResolution{
				Kind: TargetUnresolvable,
				Detail: fmt.Sprintf(
					"Backup %q named by bootstrap.recovery.backup was not found in namespace %q (%v)",
					name, namespace, err),
			}, nil
		}
		return resolutionFromBackup(TargetBackupName, backup, fmt.Sprintf("Backup %s", name))
	}

	// Recovery with no target at all replays every WAL segment available, so
	// the newest configuration is the matching one.
	return &TargetResolution{
		Kind:   TargetNone,
		Detail: "the recovery has no target, so it restores to the end of the archive",
	}, nil
}

func resolutionFromBackup(kind TargetKind, backup *apiv1.Backup, detail string) (*TargetResolution, error) {
	if backup.Status.StoppedAt == nil {
		return &TargetResolution{
			Kind: TargetUnresolvable,
			Detail: fmt.Sprintf(
				"%s has not recorded a stoppedAt time, so the moment it corresponds to is unknown",
				detail),
		}, nil
	}
	moment := backup.Status.StoppedAt.Time.UTC()
	return &TargetResolution{
		Kind: kind, Moment: moment, HasMoment: true,
		Detail: fmt.Sprintf("%s (%s)", detail, moment.Format(time.RFC3339)),
	}, nil
}

// stringAt reads a non-empty string at a path.
func stringAt(object map[string]any, path string) (string, bool) {
	raw, found := pathutil.Get(object, path)
	if !found {
		return "", false
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}
