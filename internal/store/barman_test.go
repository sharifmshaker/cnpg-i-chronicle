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

package store

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/sharifmshaker/cnpg-i-chronicle/internal/pathutil"
)

func barmanSpecConfiguration(t *testing.T, body string) map[string]any {
	t.Helper()
	raw, err := pathutil.FromJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var testKey = types.NamespacedName{Namespace: "default", Name: "backup-store"}

// A real barman ObjectStore carries a great deal this plugin has no use for.
// Deriving must take the four fields that matter and leave the rest behind.
func TestDeriveNarrowsABarmanConfiguration(t *testing.T) {
	raw := barmanSpecConfiguration(t, `{
	  "destinationPath": "s3://backups/team/prod",
	  "endpointURL": "http://backups:9000",
	  "endpointCA": {"name": "minio-tls", "key": "tls.crt"},
	  "s3Credentials": {
	    "accessKeyId": {"name": "backups", "key": "ACCESS_KEY_ID"},
	    "secretAccessKey": {"name": "backups", "key": "ACCESS_SECRET_KEY"},
	    "region": {"name": "backups", "key": "REGION"}
	  },
	  "wal": {"compression": "gzip", "encryption": "AES256", "maxParallel": 8},
	  "data": {"compression": "bzip2", "jobs": 4, "immediateCheckpoint": true},
	  "tags": {"team": "platform"},
	  "historyTags": {"retention": "long"},
	  "serverName": "pg-prod"
	}`)

	got, err := deriveConfiguration(raw, testKey)
	if err != nil {
		t.Fatal(err)
	}

	if got.DestinationPath != "s3://backups/team/prod" {
		t.Errorf("destinationPath = %q", got.DestinationPath)
	}
	if got.EndpointURL != "http://backups:9000" {
		t.Errorf("endpointURL = %q", got.EndpointURL)
	}
	if got.EndpointCA == nil || got.EndpointCA.Name != "minio-tls" || got.EndpointCA.Key != "tls.crt" {
		t.Errorf("endpointCA = %+v", got.EndpointCA)
	}
	if got.S3Credentials.AccessKeyID == nil || got.S3Credentials.AccessKeyID.Key != "ACCESS_KEY_ID" {
		t.Errorf("accessKeyId = %+v", got.S3Credentials.AccessKeyID)
	}
	if got.S3Credentials.Region == nil || got.S3Credentials.Region.Key != "REGION" {
		t.Errorf("region = %+v", got.S3Credentials.Region)
	}

	// The narrow type has nowhere to put wal, data, tags, historyTags or
	// serverName, which is the point: they cannot leak into our CRD or be
	// mistaken for settings this plugin honours.
	if got.S3Credentials.InheritFromIAMRole {
		t.Error("inheritFromIAMRole should be false when not set")
	}
}

func TestDeriveSupportsIAMRole(t *testing.T) {
	raw := barmanSpecConfiguration(t, `{
	  "destinationPath": "s3://backups/",
	  "s3Credentials": {"inheritFromIAMRole": true}
	}`)

	got, err := deriveConfiguration(raw, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !got.S3Credentials.InheritFromIAMRole {
		t.Error("inheritFromIAMRole was dropped")
	}
}

// A provider this plugin cannot talk to must be reported when the store is
// resolved, so the ConfigStore explains itself, rather than several layers
// down when a snapshot is first written.
func TestDeriveRejectsUnsupportedProvidersEarly(t *testing.T) {
	for field, provider := range map[string]string{
		"azureCredentials":  "Azure",
		"googleCredentials": "Google",
	} {
		raw := barmanSpecConfiguration(t, `{
		  "destinationPath": "s3://backups/",
		  "`+field+`": {"inheritFromAzureAD": true}
		}`)

		_, err := deriveConfiguration(raw, testKey)
		if err == nil {
			t.Errorf("%s: expected an error", field)
			continue
		}
		if !strings.Contains(err.Error(), provider) {
			t.Errorf("%s: error does not name the provider: %v", field, err)
		}
		// Derived from the fixture rather than written out, so renaming the
		// store cannot silently weaken this assertion.
		if !strings.Contains(err.Error(), testKey.Name) {
			t.Errorf("%s: error does not name the store %q: %v", field, testKey.Name, err)
		}
	}
}

func TestDeriveRequiresADestination(t *testing.T) {
	raw := barmanSpecConfiguration(t, `{"s3Credentials": {"inheritFromIAMRole": true}}`)
	if _, err := deriveConfiguration(raw, testKey); err == nil {
		t.Fatal("a configuration with no destinationPath should be rejected")
	}
}

// Extension readiness: until another backend lands, a store this plugin cannot
// talk to must fail with an explanation rather than a type error or a silent
// no-op. derivedFrom bypasses our own CRD validation, so this is the only guard
// on that path.
func TestDeriveAzureDestinationIsReportedClearly(t *testing.T) {
	raw := barmanSpecConfiguration(t, `{
	  "destinationPath": "azure://container/prefix",
	  "azureCredentials": {"inheritFromAzureAD": true}
	}`)

	_, err := deriveConfiguration(raw, testKey)
	if err == nil {
		t.Fatal("an Azure ObjectStore should be rejected when the store is resolved")
	}
	if !strings.Contains(err.Error(), "Azure") || !strings.Contains(err.Error(), "not support") {
		t.Errorf("error should say which provider and that it is unsupported: %v", err)
	}
}

// An azure:// destination with no credential block gets past deriveConfiguration
// and must still fail comprehensibly, at the point a backend is built.
func TestUnsupportedSchemeFailsAtBackend(t *testing.T) {
	destination, err := ParseDestination("azure://container/prefix")
	if err != nil {
		t.Fatalf("ParseDestination already understands the scheme: %v", err)
	}
	if destination.Scheme != "azure" || destination.Bucket != "container" {
		t.Fatalf("parsed = %+v", destination)
	}

	resolver := &Resolver{}
	_, err = resolver.Backend(context.Background(), "default", &Resolved{Destination: destination})
	if err == nil {
		t.Fatal("building a backend for an unimplemented scheme should fail")
	}
	if !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("unhelpful error: %v", err)
	}
}
