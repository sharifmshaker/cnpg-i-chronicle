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

// Package v1 contains the API Schema definitions for the chronicle v1 API group.
// +kubebuilder:object:generate=true
// +groupName=chronicle.sharifmshaker.github.io
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "chronicle.sharifmshaker.github.io", Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	//
	// This uses apimachinery's SchemeBuilder rather than controller-runtime's
	// scheme.Builder, which is deprecated precisely because an api package
	// should be cheap to import: depending on controller-runtime here would
	// drag the whole manager machinery into anything that only wants the types.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers every type in this group-version.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&ConfigStore{}, &ConfigStoreList{},
		&RestorePolicy{}, &RestorePolicyList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
