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

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeBackupLookup resolves Backup resources through the API server.
type KubeBackupLookup struct {
	Client client.Reader
}

// ByName returns the Backup with this name.
func (l KubeBackupLookup) ByName(
	ctx context.Context,
	namespace, name string,
) (*apiv1.Backup, error) {
	var backup apiv1.Backup
	if err := l.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// ByBackupID finds the Backup whose status recorded this backup id.
//
// A backup id is assigned by the backup tool, not by Kubernetes, so it can only
// be found by scanning. This deliberately does not reach into the object store
// to read barman's own metadata: doing so would mean parsing a file format this
// plugin has no way to exercise in tests, and the Backup resource already
// records the same instant. The cost is that a backupID belonging to a cluster
// whose Backup resources live elsewhere cannot be resolved — which surfaces as
// an explicit unresolvable target, not a wrong answer.
func (l KubeBackupLookup) ByBackupID(
	ctx context.Context,
	namespace, backupID string,
) (*apiv1.Backup, error) {
	var backups apiv1.BackupList
	if err := l.Client.List(ctx, &backups, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for i := range backups.Items {
		backup := &backups.Items[i]
		if backup.Status.BackupID == backupID {
			return backup, nil
		}
		// Barman 3.3+ also lets a backup be addressed by its name.
		if backup.Status.BackupName != "" && backup.Status.BackupName == backupID {
			return backup, nil
		}
	}
	return nil, fmt.Errorf("no Backup in namespace %q has status.backupId %q", namespace, backupID)
}
