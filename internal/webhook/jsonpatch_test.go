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

package webhook

import jsonpatch "github.com/evanphx/json-patch/v5"

// jsonpatchDecode is a thin alias so the test file reads without importing the
// patch library alongside its Kubernetes imports.
func jsonpatchDecode(raw []byte) (jsonpatch.Patch, error) { return jsonpatch.DecodePatch(raw) }
