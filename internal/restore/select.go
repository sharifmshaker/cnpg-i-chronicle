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

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/snapshot"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// Selector reads snapshots for one source cluster.
type Selector interface {
	List(ctx context.Context) ([]store.SnapshotRef, error)
	Read(ctx context.Context, key string) (*snapshot.Snapshot, error)
	Latest(ctx context.Context) (*snapshot.Snapshot, string, error)
}

// Alignment is the context AlignWithRecoveryTarget needs: the Cluster being
// created, and a way to look up the Backup resources it refers to. Only the
// admission webhook has one, and AlignWithRecoveryTarget refuses to run
// without it rather than guess.
type Alignment struct {
	Cluster   map[string]any
	Namespace string
	Backups   BackupLookup
}

// Resolution is a chosen snapshot and the key it lives under, so the restore's
// audit trail names a real object.
type Resolution struct {
	Snapshot *snapshot.Snapshot
	Key      string

	// Target explains how the moment was derived, when the policy aligned with
	// a recovery target. Nil for the other modes.
	Target *TargetResolution
}

// Select resolves one snapshot from a history.
//
// The mode is passed in rather than read from the policy because a policy is
// optional and carries no source: the default depends on the Cluster being
// created, not on the rules being applied. See chroniclev1.ResolveMode.
func Select(
	ctx context.Context,
	source Selector,
	mode chroniclev1.SelectMode,
	policy *chroniclev1.RestorePolicy,
	alignment *Alignment,
) (*Resolution, error) {
	switch mode {
	case chroniclev1.SelectLatest:
		snap, key, err := source.Latest(ctx)
		if err != nil {
			return nil, err
		}
		return &Resolution{Snapshot: snap, Key: key}, nil

	case chroniclev1.SelectGeneration:
		// Only a policy can ask for this mode, so a nil policy here would be a
		// programming error rather than a configuration one. Guarded anyway:
		// this runs inside admission, where a panic takes out cluster creation
		// for everyone.
		if policy == nil || policy.Spec.Select.Generation == nil {
			return nil, fmt.Errorf("select.mode is Generation but select.generation is unset")
		}
		return selectByGeneration(ctx, source, *policy.Spec.Select.Generation)

	case chroniclev1.SelectTimestamp:
		if policy == nil || policy.Spec.Select.Timestamp == nil {
			return nil, fmt.Errorf("select.mode is Timestamp but select.timestamp is unset")
		}
		return SelectAtOrBefore(ctx, source, policy.Spec.Select.Timestamp.Time.UTC())

	case chroniclev1.SelectAlignWithRecoveryTarget:
		return selectAligned(ctx, source, policy, alignment)

	default:
		return nil, fmt.Errorf("unknown select mode %q", mode)
	}
}

func selectByGeneration(ctx context.Context, source Selector, generation int64) (*Resolution, error) {
	refs, err := source.List(ctx)
	if err != nil {
		return nil, err
	}

	// Refs are ordered oldest first. A generation can appear more than once,
	// because label edits are captured without bumping generation; the newest
	// capture at that generation is the most complete one.
	var chosen *store.SnapshotRef
	for i := range refs {
		if refs[i].Generation == generation {
			chosen = &refs[i]
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf(
			"no snapshot for generation %d (%d snapshots available%s)",
			generation, len(refs), describeRange(refs))
	}
	snap, err := source.Read(ctx, chosen.Key)
	if err != nil {
		return nil, err
	}
	return &Resolution{Snapshot: snap, Key: chosen.Key}, nil
}

// SelectAtOrBefore returns the newest snapshot captured at or before a moment.
//
// This is the primitive point-in-time restore builds on: given a recovery
// target time, the configuration in force at that moment is the last one
// captured before it.
func SelectAtOrBefore(
	ctx context.Context,
	source Selector,
	moment time.Time,
) (*Resolution, error) {
	refs, err := source.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: the source has no snapshots", store.ErrNotFound)
	}

	moment = moment.UTC()

	// Refs are ordered oldest first, so the last one that is not after the
	// moment is the answer. "At" counts as before: a snapshot captured exactly
	// at the target time describes the configuration at that time.
	var chosen *store.SnapshotRef
	for i := range refs {
		if refs[i].CapturedAt.After(moment) {
			break
		}
		chosen = &refs[i]
	}

	if chosen == nil {
		return nil, fmt.Errorf(
			"no snapshot captured at or before %s; the earliest is %s. The source "+
				"cluster's configuration history does not reach back that far",
			moment.Format(time.RFC3339), refs[0].CapturedAt.Format(time.RFC3339))
	}
	snap, err := source.Read(ctx, chosen.Key)
	if err != nil {
		return nil, err
	}
	return &Resolution{Snapshot: snap, Key: chosen.Key}, nil
}

// describeRange summarises the available generations for an error message.
func describeRange(refs []store.SnapshotRef) string {
	if len(refs) == 0 {
		return ""
	}
	lowest, highest := refs[0].Generation, refs[0].Generation
	for _, ref := range refs {
		if ref.Generation < lowest {
			lowest = ref.Generation
		}
		if ref.Generation > highest {
			highest = ref.Generation
		}
	}
	return fmt.Sprintf(", generations %d to %d", lowest, highest)
}

// selectAligned derives the moment from the Cluster's recovery target.
func selectAligned(
	ctx context.Context,
	source Selector,
	policy *chroniclev1.RestorePolicy,
	alignment *Alignment,
) (*Resolution, error) {
	if alignment == nil || alignment.Cluster == nil {
		return nil, fmt.Errorf(
			"select.mode is AlignWithRecoveryTarget, which resolves against the Cluster " +
				"being created and cannot be evaluated without one")
	}

	target, err := ResolveRecoveryTarget(ctx, alignment.Cluster, alignment.Namespace, alignment.Backups)
	if err != nil {
		return nil, err
	}

	switch {
	case target.HasMoment:
		resolution, err := SelectAtOrBefore(ctx, source, target.Moment)
		if err != nil {
			return nil, fmt.Errorf("aligning with %s: %w", target.Detail, err)
		}
		resolution.Target = target
		return resolution, nil

	case target.Kind == TargetNone:
		// Recovering to the end of the archive means the newest configuration
		// is the matching one.
		snap, key, err := source.Latest(ctx)
		if err != nil {
			return nil, err
		}
		return &Resolution{Snapshot: snap, Key: key, Target: target}, nil

	default:
		if policy.GetOnUnresolvable() == chroniclev1.UnresolvableUseLatest {
			snap, key, err := source.Latest(ctx)
			if err != nil {
				return nil, err
			}
			return &Resolution{Snapshot: snap, Key: key, Target: target}, nil
		}
		return nil, fmt.Errorf(
			"cannot align the configuration with this cluster's recovery target: %s. "+
				"Set select.mode to Timestamp with the instant you are recovering to, or set "+
				"select.onUnresolvable to UseLatest to accept the most recent configuration",
			target.Detail)
	}
}
