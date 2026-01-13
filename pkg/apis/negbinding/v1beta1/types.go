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
	NetworkEndpointGroups []RequestedNegRef `json:"networkEndpointGroups"`
}

// BackendRefConfig specifies backendRef for the NetworkEndpointGroupBinding resource
// +k8s:openapi-gen=true
type BackendRefConfig struct {
	Group string         `json:"group"`
	Kind  BackendRefKind `json:"kind"`
	Name  string         `json:"name"`
	Port  int            `json:"port"`
}

// +k8s:openapi-gen=true
type BackendRefKind string

const (
	ServiceKind = BackendRefKind("Service")
)

// +k8s:openapi-gen=true
// RequestedNegRef references NetworkEndpointGroups across cluster zones which should be managed by controller
type RequestedNegRef struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
}

// NetworkEndpointGroupBindingStatus is the status for a BackendConfig resource
type NetworkEndpointGroupBindingStatus struct {
	// Conditions describe the current conditions of the NetworkEndpointGroupBinding.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Negs contains currently managed NEGs referenced by NetworkEndpointGroupsBinding
	// +listType=map
	// +listMapKey=id
	NetworkEndpointGroups []StatusNegRef `json:"networkEndpointGroups,omitempty"`
}

// StatusNegRef references NetworkEndpointGroup which is actively managed by controller
type StatusNegRef struct {
	// The unique identifier for the NEG resource in GCE API.
	Id string `json:"id"`

	// SelfLink is the GCE Server-defined fully-qualified URL for the GCE NEG resource
	SelfLink string `json:"selfLink"`

	// URL of the subnetwork to which all network endpoints in the NEG belong.
	SubnetURL string `json:"subnetURL"`

	// NetworkEndpointType: Type of network endpoints in this network
	// endpoint group.
	NetworkEndpointType NetworkEndpointType `json:"networkEndpointType"`
}

// +k8s:openapi-gen=true
type NetworkEndpointType string

const (
	VmIpPortEndpointType = NetworkEndpointType("GCE_VM_IP_PORT")
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// NetworkEndpointGroupBindingList is a list of NetworkEndpointGroupBinding resources
type NetworkEndpointGroupBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []NetworkEndpointGroupBinding `json:"items"`
}
