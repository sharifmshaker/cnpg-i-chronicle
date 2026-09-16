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
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"

	chroniclev1 "github.com/sharifmshaker/cnpg-i-chronicle/api/v1"
)

// storeConfiguration is a minimal inline object store configuration for tests.
func storeConfiguration() chroniclev1.ObjectStoreConfiguration {
	return chroniclev1.ObjectStoreConfiguration{
		DestinationPath: "s3://backups/",
		EndpointURL:     "http://backups:9000",
		S3Credentials: &chroniclev1.S3Credentials{
			AccessKeyID: &machineryapi.SecretKeySelector{
				LocalObjectReference: machineryapi.LocalObjectReference{Name: "backup-store"},
				Key:                  "ACCESS_KEY_ID",
			},
			SecretAccessKey: &machineryapi.SecretKeySelector{
				LocalObjectReference: machineryapi.LocalObjectReference{Name: "backup-store"},
				Key:                  "ACCESS_SECRET_KEY",
			},
		},
	}
}
