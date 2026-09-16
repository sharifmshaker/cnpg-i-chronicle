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

package operator

import (
	"context"

	"github.com/cloudnative-pg/cnpg-i-machinery/pkg/pluginhelper/http"
	"github.com/cloudnative-pg/cnpg-i/pkg/operator"
	"github.com/cloudnative-pg/cnpg-i/pkg/reconciler"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/identity"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
)

// CNPGI serves the plugin's gRPC surface to the CloudNativePG operator.
//
// It drives pluginhelper's http.Server directly rather than going through
// CreateMainCmd, because every hook needs a Kubernetes client and the
// convenience constructor gives no way to inject one.
type CNPGI struct {
	Client   client.Client
	Resolver *store.Resolver

	// PluginPath is the directory for a unix socket, used when the plugin runs
	// as a sidecar of the operator.
	PluginPath string

	// The remaining fields configure the mTLS TCP listener used by the
	// standalone Deployment. The operator discovers it through the Service's
	// cnpg.io/pluginName label and its pluginPort, pluginClientSecret and
	// pluginServerSecret annotations.
	ServerCertPath string
	ServerKeyPath  string
	ClientCertPath string
	ServerAddress  string
}

// NeedLeaderElection reports that the plugin's gRPC server does not need to be
// the leader to serve.
//
// Without this method controller-runtime falls back to treating any Runnable as
// leader-election-dependent ("for backwards compatibility", runnable_group.go),
// which would leave a non-leader replica passing its readiness probe and joining
// the Service endpoints while nothing listens on the gRPC port. The operator
// would then dial a dead port.
//
// Serving is safe on every replica: the hooks are request-scoped and are called
// by the CloudNativePG operator, which is itself leader-elected, so there is
// exactly one caller and no duplicate invocations to guard against.
func (c *CNPGI) NeedLeaderElection() bool { return false }

// Start serves until the context is cancelled.
func (c *CNPGI) Start(ctx context.Context) error {
	// One cache for both hooks: capture confirms a state is settled, and the
	// status hook needs that same answer to stop polling the store for it.
	settled := newSettledCache()
	enrich := func(server *grpc.Server) error {
		reconciler.RegisterReconcilerHooksServer(server, ReconcilerImplementation{
			Client:   c.Client,
			Resolver: c.Resolver,
			Settled:  settled,
		})
		operator.RegisterOperatorServer(server, OperatorImplementation{
			Client:   c.Client,
			Resolver: c.Resolver,
			Settled:  settled,
		})
		return nil
	}

	server := http.Server{
		IdentityImpl:   identity.Implementation{},
		Enrichers:      []http.ServerEnricher{enrich},
		PluginPath:     c.PluginPath,
		ServerCertPath: c.ServerCertPath,
		ServerKeyPath:  c.ServerKeyPath,
		ClientCertPath: c.ClientCertPath,
		ServerAddress:  c.ServerAddress,
	}
	return server.Start(ctx)
}
