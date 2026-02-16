/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
//
// +k8s:openapi-gen=true
type NetworkEndpointGroupBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkEndpointGroupBindingSpec   `json:"spec,omitempty"`
	Status NetworkEndpointGroupBindingStatus `json:"status,omitempty"`
}

// NetworkEndpointGroupBindingSpec is the spec for a NetworkEndpointGroupBinding resource
// +k8s:openapi-gen=true
type NetworkEndpointGroupBindingSpec struct {
	// BackendRef references backend to which NEGs should be attached
	// +k8s:validation:cel[0]:rule="oldSelf == self"
	// +k8s:validation:cel[0]:message="BackendRef can't be changed"
	BackendRef *BackendRefConfig `json:"backendRef"`
	// NetworkEndpointGroups references NEGs which should be managed by controller
	// +listType=map
	// +listMapKey=name
	// +k8s:validation:maxItems=50
	// +k8s:validation:cel[0]:rule="self.all(i, self.filter(j, j.subnet == i.subnet).size() <= 1)"
	// +k8s:validation:cel[0]:message="Single NEGBinding can manage only single NEG per subnet"
	NetworkEndpointGroups []SpecNegRef `json:"networkEndpointGroups"`
}

// BackendRefConfig specifies backendRef for the NetworkEndpointGroupBinding resource
// +k8s:openapi-gen=true
type BackendRefConfig struct {
	Group string         `json:"group"`
	Kind  BackendRefKind `json:"kind"`
	Name  string         `json:"name"`
	Port  int32          `json:"port"`
}

// +k8s:openapi-gen=true
type BackendRefKind string

const (
	ServiceKind = BackendRefKind("Service")
)

// +k8s:openapi-gen=true
// SpecNegRef references NetworkEndpointGroups across cluster zones which should be managed by controller
type SpecNegRef struct {
	Name string `json:"name"`
	// +k8s:validation:maxLength=63
	Subnet string `json:"subnet"`
}

// +k8s:openapi-gen=true
// NetworkEndpointGroupBindingStatus is the status for a BackendConfig resource
type NetworkEndpointGroupBindingStatus struct {
	// Last time the NEG syncer syncs associated NEGs.
	// +optional
	LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`
	// Conditions describe the current conditions of the NetworkEndpointGroupBinding.
	// +listType=map
	// +listMapKey=type
	Conditions []Condition `json:"conditions,omitempty"`
	// Negs contains currently managed NEGs referenced by NetworkEndpointGroupsBinding
	// +listType=map
	// +listMapKey=resourceURL
	NetworkEndpointGroups []StatusNegRef `json:"networkEndpointGroups,omitempty"`
}

// +k8s:openapi-gen=true
type Condition metav1.Condition

// +k8s:openapi-gen=true
// StatusNegRef references NetworkEndpointGroup which is actively managed by controller
type StatusNegRef struct {
	// ResourceURL is the GCE Server-defined fully-qualified URL for the GCE NEG resource
	ResourceURL string `json:"resourceURL"`

	// URL of the subnetwork to which all network endpoints in the NEG belong.
	SubnetURL string `json:"subnetURL"`

	// Current state of Neg: ACTIVE - actively managing endpoints, CLEANING_UP - currently removing all endpoints, CLEANED_UP - all endpoints detached
	State NegState `json:"state"`
}

// +k8s:openapi-gen=true
type NegState string

const (
	NegStateActive     = NegState("ACTIVE")
	NegStateCleaningUp = NegState("CLEANING_UP")
	NegStateCleanedUp  = NegState("CLEANED_UP")
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// NetworkEndpointGroupBindingList is a list of NetworkEndpointGroupBinding resources
type NetworkEndpointGroupBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []NetworkEndpointGroupBinding `json:"items"`
}
