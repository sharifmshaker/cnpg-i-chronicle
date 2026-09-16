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

package identity

import (
	"context"
	"testing"

	"github.com/cloudnative-pg/cnpg-i/pkg/identity"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/cnpgi/metadata"
)

// The name here must match the plugin name a Cluster puts in spec.plugins[],
// and the one the webhook's matchConditions keys on. A mismatch means the
// operator never routes anything to this plugin.
func TestPluginMetadataMatchesTheConfiguredName(t *testing.T) {
	got, err := Implementation{}.GetPluginMetadata(
		context.Background(), &identity.GetPluginMetadataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != metadata.PluginName {
		t.Errorf("plugin name = %q, want %q", got.Name, metadata.PluginName)
	}
	if got.Version == "" {
		t.Error("the plugin reports no version")
	}
}

// Probe is what the operator uses to decide the plugin is reachable.
func TestProbeReportsReady(t *testing.T) {
	got, err := Implementation{}.Probe(context.Background(), &identity.ProbeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ready {
		t.Error("Probe reported not ready")
	}
}
