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

// Package operator implements the CNPG-I services the plugin serves to the
// CloudNativePG operator.
//
// The name is the deployment locus, not the Operator RPC. A CNPG-I plugin
// conventionally splits into an operator-side component, running beside the
// CloudNativePG operator, and an instance-side sidecar running in each Postgres
// pod. This plugin has no sidecar, so every service it serves lives here:
//
//   - ReconcilerHooks, in reconciler.go. Pre refuses a Cluster whose restore
//     never ran; Post captures a snapshot once the spec has settled.
//   - Operator, in status.go, of which only SetStatusInCluster is implemented.
//     It publishes the capture watermark. MutateCluster is left out because
//     CloudNativePG never calls it — which is why restore runs in an admission
//     webhook instead, in internal/webhook.
//
// The watermark is the seam between the two: SetStatusInCluster publishes it
// and the Post hook reads it back through WatermarkFromCluster, the cheapest
// path capture has.
//
// Identity is served from internal/cnpgi/identity, and start.go holds the
// manager Runnable that registers all of them on one gRPC server.
package operator
