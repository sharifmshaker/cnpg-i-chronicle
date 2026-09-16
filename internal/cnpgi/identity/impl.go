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

// Package identity implements the CNPG-I Identity service.
package identity

import (
	"context"

	"github.com/cloudnative-pg/cnpg-i/pkg/identity"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
)

// Implementation answers the Identity service.
type Implementation struct {
	identity.UnimplementedIdentityServer
}

// GetPluginMetadata returns the plugin's identity.
func (Implementation) GetPluginMetadata(
	_ context.Context,
	_ *identity.GetPluginMetadataRequest,
) (*identity.GetPluginMetadataResponse, error) {
	return &metadata.Data, nil
}

// GetPluginCapabilities declares which services the operator should call.
//
// Only the reconciler hooks and the operator service are advertised. Restore
// runs through this plugin's own admission webhook rather than CNPG-I, because
// the protocol's create-time hook (Operator.MutateCluster) has had no caller in
// the operator since remote plugin support landed.
func (Implementation) GetPluginCapabilities(
	_ context.Context,
	_ *identity.GetPluginCapabilitiesRequest,
) (*identity.GetPluginCapabilitiesResponse, error) {
	return &identity.GetPluginCapabilitiesResponse{
		Capabilities: []*identity.PluginCapability{
			{
				Type: &identity.PluginCapability_Service_{
					Service: &identity.PluginCapability_Service{
						Type: identity.PluginCapability_Service_TYPE_RECONCILER_HOOKS,
					},
				},
			},
			{
				Type: &identity.PluginCapability_Service_{
					Service: &identity.PluginCapability_Service{
						Type: identity.PluginCapability_Service_TYPE_OPERATOR_SERVICE,
					},
				},
			},
		},
	}, nil
}

// Probe reports readiness.
func (Implementation) Probe(
	_ context.Context,
	_ *identity.ProbeRequest,
) (*identity.ProbeResponse, error) {
	return &identity.ProbeResponse{Ready: true}, nil
}
