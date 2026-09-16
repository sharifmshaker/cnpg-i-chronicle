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

// Package operator wires up the plugin's manager and gRPC server.
package operator

import (
	"fmt"

	apiv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
	cnpgioperator "github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/operator"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/controller"
	"github.com/sharifmshaker/cnpg-i-chronicle/internal/store"
	chroniclewebhook "github.com/sharifmshaker/cnpg-i-chronicle/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(chroniclev1.AddToScheme(scheme))
	utilruntime.Must(apiv1.AddToScheme(scheme))
}

// NewCmd builds the "operator" subcommand.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Run the plugin's operator-side services",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd)
		},
	}

	cmd.Flags().String("server-cert", "", "TLS certificate presented to the CloudNativePG operator")
	cmd.Flags().String("server-key", "", "Key for the server certificate")
	cmd.Flags().String("client-cert", "", "CA used to verify the operator's client certificate")
	cmd.Flags().String("server-address", "", "Address for the gRPC listener, e.g. :9090")
	cmd.Flags().String("plugin-path", "", "Directory for a unix socket, when running as an operator sidecar")
	cmd.Flags().Int("webhook-port", 9443, "Port for the restore admission webhook")
	cmd.Flags().String("webhook-cert-dir", "/server", "Directory holding the webhook's tls.crt and tls.key")
	cmd.Flags().String("metrics-bind-address", "0", "Address for the metrics endpoint, 0 to disable")
	cmd.Flags().String("health-probe-bind-address", ":8081", "Address for the health probe endpoint")
	cmd.Flags().Bool("leader-elect", false, "Enable leader election")

	// TCP and unix-socket serving are alternatives, and the three TLS flags are
	// meaningless individually.
	cmd.MarkFlagsRequiredTogether("server-cert", "server-key", "client-cert", "server-address")
	cmd.MarkFlagsMutuallyExclusive("server-cert", "plugin-path")

	utilruntime.Must(viper.BindPFlags(cmd.Flags()))
	viper.AutomaticEnv()

	return cmd
}

func run(cmd *cobra.Command) error {
	ctx := cmd.Context()
	contextLogger := log.FromContext(ctx)

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: viper.GetString("metrics-bind-address")},
		HealthProbeBindAddress: viper.GetString("health-probe-bind-address"),
		LeaderElection:         viper.GetBool("leader-elect"),
		LeaderElectionID:       "chronicle.sharifmshaker.github.io",

		// Release the lease on shutdown so a rolling update hands over
		// immediately instead of the incoming replica waiting out the lease
		// duration. That wait matters more here than in a plain controller:
		// losing leadership stops the manager, and the manager also serves the
		// restore webhook, which is registered failurePolicy: Fail.
		LeaderElectionReleaseOnCancel: true,

		// The restore webhook shares the plugin's serving certificate: the
		// Service fronts both the gRPC port and the webhook port, so one
		// cert-manager Certificate covers both.
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    viper.GetInt("webhook-port"),
			CertDir: viper.GetString("webhook-cert-dir"),
		}),

		Client: client.Options{
			Cache: &client.CacheOptions{
				// Secrets and Clusters are read rarely — Secrets only when an
				// object store is actually opened, Clusters only when the
				// ConfigStore controller enforces retention every few minutes.
				// Caching them would mirror every Secret and every Cluster in
				// the fleet into this process's memory to save reads it barely
				// makes.
				DisableFor: []client.Object{&corev1.Secret{}, &apiv1.Cluster{}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("while creating the manager: %w", err)
	}

	resolver := &store.Resolver{
		Client: manager.GetClient(),
		// The barman ObjectStore CRD may legitimately be absent, so its reads go
		// through the uncached API reader. An informer on a missing CRD fails
		// manager startup outright.
		ObjectStores: store.NewObjectStoreReader(manager.GetAPIReader(), store.DefaultObjectStoreTTL),
	}

	if err := (&controller.ConfigStoreReconciler{
		Client:   manager.GetClient(),
		Resolver: resolver,
	}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("while setting up the ConfigStore controller: %w", err)
	}

	// No Resolver: policy validation reads no object store.
	if err := (&controller.RestorePolicyReconciler{
		Client: manager.GetClient(),
	}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("while setting up the RestorePolicy controller: %w", err)
	}

	pluginServer := &cnpgioperator.CNPGI{
		Client:         manager.GetClient(),
		Resolver:       resolver,
		PluginPath:     viper.GetString("plugin-path"),
		ServerCertPath: viper.GetString("server-cert"),
		ServerKeyPath:  viper.GetString("server-key"),
		ClientCertPath: viper.GetString("client-cert"),
		ServerAddress:  viper.GetString("server-address"),
	}
	if err := manager.Add(pluginServer); err != nil {
		return fmt.Errorf("while registering the plugin server: %w", err)
	}

	// Admission runs on every leader and non-leader replica alike: the API
	// server load-balances across endpoints and does not know about leases.
	manager.GetWebhookServer().Register("/mutate-postgresql-cnpg-io-v1-cluster", &admission.Webhook{
		Handler: &chroniclewebhook.ClusterRestorer{
			// The uncached reader, deliberately: a RestorePolicy applied in
			// the same manifest as the Cluster that references it must be
			// visible immediately. See the field comment on ClusterRestorer.
			Client:   manager.GetAPIReader(),
			Resolver: resolver,
		},
	})

	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("while adding the health check: %w", err)
	}
	// Readiness must mean "can actually serve", not "the process is alive".
	//
	// A plain ping would mark the pod Ready before the webhook is listening. The
	// API server would then route an admission request to it, and because the
	// restore webhook is registered failurePolicy: Fail, that turns a normal
	// startup into blocked cluster creation. StartedChecker dials the webhook
	// port, so the pod joins the Service endpoints only once it can answer.
	if err := manager.AddReadyzCheck("webhook", manager.GetWebhookServer().StartedChecker()); err != nil {
		return fmt.Errorf("while adding the webhook readiness check: %w", err)
	}

	contextLogger.Info("starting cnpg-i-chronicle")
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("while running the manager: %w", err)
	}
	return nil
}
