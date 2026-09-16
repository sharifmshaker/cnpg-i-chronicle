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

package snapshot

import "sort"

// MetadataGroupName is the one group whose coverage extends outside the spec:
// it also captures the Cluster's own labels and annotations, which live under
// metadata rather than under any path a Group can name.
const MetadataGroupName = "Metadata"

// Group is a named, individually skippable set of Cluster fields.
//
// Groups exist so that a restore policy can say "skip Resources" instead of
// enumerating every path underneath it, while still allowing path-level
// precision when someone needs it.
type Group struct {
	// Name is the token used in a RestorePolicy skip rule.
	Name string

	// Description is surfaced in docs and validation errors.
	Description string

	// Paths are the dotted paths this group covers.
	Paths []string

	// DefaultEnabled is false for groups that are unsafe to move between
	// clusters without the operator opting in.
	DefaultEnabled bool
}

// Groups is the capture allowlist.
//
// This is an allowlist, not a denylist, and that is the primary safety
// mechanism: a field CloudNativePG adds in a future release is not captured
// until someone deliberately adds it here. The hard denylist in denylist.go is
// defence in depth on top of it.
var Groups = []Group{
	{
		Name: "Gucs",
		Description: "PostgreSQL configuration: parameters, pg_hba, pg_ident, " +
			"shared_preload_libraries, synchronous replication, and the pod " +
			"selectors pg_hba rules can reference",
		Paths: []string{
			"spec.postgresql",

			// podSelectorRefs sits on ClusterSpec rather than under
			// postgresql, but it belongs to this group: a pg_hba rule may
			// address a client set as ${podselector:NAME}, and NAME is defined
			// here. Capturing the rules without the selectors they reference
			// would restore host-based authentication that points at nothing.
			"spec.podSelectorRefs",
		},
		DefaultEnabled: true,
	},
	{
		Name:           "Resources",
		Description:    "CPU and memory requests and limits",
		Paths:          []string{"spec.resources"},
		DefaultEnabled: true,
	},
	{
		Name:        "Storage",
		Description: "Data, WAL and tablespace storage sizing and classes",
		Paths: []string{
			"spec.storage",
			"spec.walStorage",
			"spec.tablespaces",
		},
		DefaultEnabled: true,
	},
	{
		Name:        "Scheduling",
		Description: "Affinity, tolerations, node selectors, topology spread constraints, priority and scheduler",
		Paths: []string{
			"spec.affinity",
			"spec.topologySpreadConstraints",
			"spec.priorityClassName",
			"spec.schedulerName",
			"spec.nodeMaintenanceWindow",
		},
		DefaultEnabled: true,
	},
	{
		Name:           "Instances",
		Description:    "Replica count",
		Paths:          []string{"spec.instances"},
		DefaultEnabled: true,
	},
	{
		Name:        MetadataGroupName,
		Description: "Cluster labels and annotations, and inheritedMetadata",
		Paths: []string{
			"spec.inheritedMetadata",
		},
		DefaultEnabled: true,
	},
	{
		Name:        "Image",
		Description: "Container image, image catalog reference and pull settings",
		Paths: []string{
			"spec.imageName",
			"spec.imageCatalogRef",
			"spec.imagePullPolicy",
			"spec.imagePullSecrets",
		},
		DefaultEnabled: true,
	},
	{
		Name:           "Monitoring",
		Description:    "Custom queries and pod monitor settings",
		Paths:          []string{"spec.monitoring"},
		DefaultEnabled: true,
	},
	{
		Name:           "Managed",
		Description:    "Managed roles and services",
		Paths:          []string{"spec.managed"},
		DefaultEnabled: true,
	},
	{
		Name:        "PodEnv",
		Description: "Environment, projected volumes and service account template",
		Paths: []string{
			"spec.env",
			"spec.envFrom",
			"spec.projectedVolumeTemplate",
			"spec.serviceAccountTemplate",
			"spec.ephemeralVolumeSource",
			"spec.ephemeralVolumesSizeLimit",
		},
		DefaultEnabled: true,
	},
	{
		Name:        "Replication",
		Description: "Replication slots and synchronous replica bounds",
		Paths: []string{
			"spec.replicationSlots",
			"spec.minSyncReplicas",
			"spec.maxSyncReplicas",
		},
		DefaultEnabled: true,
	},
	{
		Name: "Timings",
		Description: "Startup, shutdown, switchover and failover delays, probes, " +
			"and primary lease timings",
		Paths: []string{
			"spec.startDelay",
			"spec.stopDelay",
			"spec.smartShutdownTimeout",
			"spec.switchoverDelay",
			"spec.failoverDelay",
			"spec.livenessProbeTimeout",
			"spec.probes",
			"spec.primaryUpdateStrategy",
			"spec.primaryUpdateMethod",

			// Four integers tuning the Lease the primary holds to coordinate
			// election. They carry no names or references, so unlike
			// spec.replica they cannot point a restored cluster at anything
			// belonging to the source; and a tuned lease profile is the same
			// kind of deliberate failover decision as switchoverDelay and
			// failoverDelay above.
			"spec.primaryLease",
		},
		DefaultEnabled: true,
	},
	{
		Name:        "Security",
		Description: "Pod and container security contexts and seccomp profile",
		Paths: []string{
			"spec.podSecurityContext",
			"spec.securityContext",
			"spec.seccompProfile",
		},
		DefaultEnabled: true,
	},
	{
		Name:        "Misc",
		Description: "Description, log level and pod disruption budget toggle",
		Paths: []string{
			"spec.description",
			"spec.logLevel",
			"spec.enablePDB",
		},
		DefaultEnabled: true,
	},

	// Opt-in groups. These are correct to restore in some topologies and
	// actively harmful in others, so they default off and must be named
	// explicitly.
	{
		Name: "Identity",
		Description: "postgresUID, postgresGID and serviceAccountName. Off by default: " +
			"UID/GID changes are a footgun against a restored PGDATA, and " +
			"serviceAccountName is immutable once the cluster exists.",
		Paths: []string{
			"spec.postgresUID",
			"spec.postgresGID",
			"spec.serviceAccountName",
		},
		DefaultEnabled: false,
	},
	{
		Name: "Secrets",
		Description: "certificates, superuserSecret and enableSuperuserAccess. Off by " +
			"default: the referenced Secrets may not exist in the target namespace.",
		Paths: []string{
			"spec.certificates",
			"spec.superuserSecret",
			"spec.enableSuperuserAccess",
		},
		DefaultEnabled: false,
	},
}

// LookupGroup returns the named group.
func LookupGroup(name string) (Group, bool) {
	for _, group := range Groups {
		if group.Name == name {
			return group, true
		}
	}
	return Group{}, false
}

// GroupNames returns every group name, sorted.
func GroupNames() []string {
	out := make([]string, 0, len(Groups))
	for _, group := range Groups {
		out = append(out, group.Name)
	}
	sort.Strings(out)
	return out
}

// DefaultGroupNames returns the names of groups captured unless told otherwise.
func DefaultGroupNames() []string {
	var out []string
	for _, group := range Groups {
		if group.DefaultEnabled {
			out = append(out, group.Name)
		}
	}
	sort.Strings(out)
	return out
}

// EffectiveGroupNames resolves a configured selection to the group names that
// will actually be captured: the configured list, or the default set when it is
// empty. Callers that need to hash or display the effective set use this rather
// than re-implementing the "empty means default" rule.
func EffectiveGroupNames(configured []string) []string {
	if len(configured) == 0 {
		return DefaultGroupNames()
	}
	return configured
}
