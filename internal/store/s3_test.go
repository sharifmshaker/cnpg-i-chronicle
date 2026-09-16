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
	"net/http"
	"strings"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// A probe failure ends up in a status condition. The SDK renders a request ID
// that differs on every attempt, and carrying it into the message would rewrite
// the ConfigStore on every resync.
func TestDescribeS3ErrorDropsRequestIDs(t *testing.T) {
	err := &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "ListObjectsV2",
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}},
				Err:      &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"},
			},
			RequestID: "REQ-3F2A9C",
		},
	}
	if !strings.Contains(err.Error(), "REQ-3F2A9C") {
		t.Fatalf("fixture no longer carries a request ID, so this test proves nothing: %v", err)
	}

	got := describeS3Error(err)
	if got != "AccessDenied: Access Denied" {
		t.Errorf("describeS3Error = %q, want the code and message alone", got)
	}
}
