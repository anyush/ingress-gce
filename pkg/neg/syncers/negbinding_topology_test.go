/*
Copyright 2026 The Kubernetes Authors.

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

package syncers

import (
	"reflect"
	"testing"
	"time"

	nodetopologyv1 "github.com/GoogleCloudPlatform/gke-networking-api/apis/nodetopology/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	negbindingv1beta1 "k8s.io/ingress-gce/pkg/apis/negbinding/v1beta1"
	fakenegbinding "k8s.io/ingress-gce/pkg/negbinding/client/clientset/versioned/fake"
	informernegbinding "k8s.io/ingress-gce/pkg/negbinding/client/informers/externalversions/negbinding/v1beta1"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/zonegetter"
	"k8s.io/klog/v2"
)

func TestNEGBindingTopologyProvider(t *testing.T) {
	namespace := "test-namespace"
	name := "test-binding"
	defaultSubnetURL := "https://www.googleapis.com/compute/v1/projects/test-project/regions/us-central1/subnetworks/default-subnet"

	testCases := []struct {
		desc            string
		initialBinding  *negbindingv1beta1.NetworkEndpointGroupBinding
		expectedSubnets []nodetopologyv1.SubnetConfig
		expectedZones   map[string][]string
		updatedBinding  *negbindingv1beta1.NetworkEndpointGroupBinding // optional runtime update
		updatedSubnets  []nodetopologyv1.SubnetConfig                  // expected after update
		updatedZones    map[string][]string                            // expected after update
	}{
		{
			desc: "Single subnet with primary default mapping",
			initialBinding: &negbindingv1beta1.NetworkEndpointGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
				},
				Spec: negbindingv1beta1.NetworkEndpointGroupBindingSpec{
					NetworkEndpointGroups: []negbindingv1beta1.SpecNegRef{
						{
							Name:   "neg-default",
							Subnet: "default-subnet",
							Zones:  []string{"us-central1-a", "us-central1-b"},
						},
					},
				},
			},
			expectedSubnets: []nodetopologyv1.SubnetConfig{
				{
					Name:       "default-subnet",
					SubnetPath: defaultSubnetURL,
				},
			},
			expectedZones: map[string][]string{
				"default-subnet": {"us-central1-a", "us-central1-b"},
			},
		},
		{
			desc: "Multiple subnets and dynamic update verification",
			initialBinding: &negbindingv1beta1.NetworkEndpointGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
				},
				Spec: negbindingv1beta1.NetworkEndpointGroupBindingSpec{
					NetworkEndpointGroups: []negbindingv1beta1.SpecNegRef{
						{
							Name:   "neg-a",
							Subnet: "subnet-a",
							Zones:  []string{"us-central1-a"},
						},
					},
				},
			},
			expectedSubnets: []nodetopologyv1.SubnetConfig{
				{
					Name:       "subnet-a",
					SubnetPath: "https://www.googleapis.com/compute/v1/projects/test-project/regions/us-central1/subnetworks/subnet-a",
				},
			},
			expectedZones: map[string][]string{
				"subnet-a": {"us-central1-a"},
			},
			updatedBinding: &negbindingv1beta1.NetworkEndpointGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
				},
				Spec: negbindingv1beta1.NetworkEndpointGroupBindingSpec{
					NetworkEndpointGroups: []negbindingv1beta1.SpecNegRef{
						{
							Name:   "neg-a",
							Subnet: "subnet-a",
							Zones:  []string{"us-central1-a", "us-central1-b"},
						},
						{
							Name:   "neg-b",
							Subnet: "subnet-b",
							Zones:  []string{"us-central1-c"},
						},
					},
				},
			},
			updatedSubnets: []nodetopologyv1.SubnetConfig{
				{
					Name:       "subnet-a",
					SubnetPath: "https://www.googleapis.com/compute/v1/projects/test-project/regions/us-central1/subnetworks/subnet-a",
				},
				{
					Name:       "subnet-b",
					SubnetPath: "https://www.googleapis.com/compute/v1/projects/test-project/regions/us-central1/subnetworks/subnet-b",
				},
			},
			updatedZones: map[string][]string{
				"subnet-a": {"us-central1-a", "us-central1-b"},
				"subnet-b": {"us-central1-c"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			fakeClient := fakenegbinding.NewSimpleClientset()
			informer := informernegbinding.NewNetworkEndpointGroupBindingInformer(fakeClient, "", time.Second, utils.NewNamespaceIndexer())
			negBindingLister := informer.GetIndexer()

			p, err := NewNEGBindingTopologyProvider(namespace, name, negBindingLister, defaultSubnetURL)
			if err != nil {
				t.Fatalf("NewNegBindingTopologyProvider() failed unexpectedly: %v", err)
			}

			negBindingLister.Add(tc.initialBinding)

			subnets := p.ListSubnets(klog.TODO())
			if !reflect.DeepEqual(subnets, tc.expectedSubnets) {
				t.Errorf("ListSubnets() returned %+v, expected %+v", subnets, tc.expectedSubnets)
			}

			zonesPerSubnet, err := p.ListZonesPerSubnet(zonegetter.AllNodesFilter, klog.TODO())
			if err != nil {
				t.Errorf("ListZonesPerSubnet() returned unexpected error: %v", err)
			}
			if !reflect.DeepEqual(zonesPerSubnet, tc.expectedZones) {
				t.Errorf("ListZonesPerSubnet() returned %+v, expected %+v", zonesPerSubnet, tc.expectedZones)
			}

			if _, ok := zonesPerSubnet["unknown-subnet"]; ok {
				t.Errorf("ListZonesPerSubnet() returned zones for unknown-subnet, expected it to be absent")
			}

			if tc.updatedBinding != nil {
				negBindingLister.Update(tc.updatedBinding)

				subnets = p.ListSubnets(klog.TODO())
				if !reflect.DeepEqual(subnets, tc.updatedSubnets) {
					t.Errorf("ListSubnets() after update returned %+v, expected %+v", subnets, tc.updatedSubnets)
				}

				zonesPerSubnet, err = p.ListZonesPerSubnet(zonegetter.AllNodesFilter, klog.TODO())
				if err != nil {
					t.Errorf("ListZonesPerSubnet() after update returned unexpected error: %v", err)
				}
				if !reflect.DeepEqual(zonesPerSubnet, tc.updatedZones) {
					t.Errorf("ListZonesPerSubnet() after update returned %+v, expected %+v", zonesPerSubnet, tc.updatedZones)
				}
			}
		})
	}

}

func TestNewNEGBindingTopologyProviderInvalidDefaultSubnetURL(t *testing.T) {
	namespace := "test-namespace"
	name := "test-binding"

	fakeClient := fakenegbinding.NewSimpleClientset()
	informer := informernegbinding.NewNetworkEndpointGroupBindingInformer(fakeClient, "", time.Second, utils.NewNamespaceIndexer())
	negBindingLister := informer.GetIndexer()

	_, err := NewNEGBindingTopologyProvider(namespace, name, negBindingLister, "invalid-url-with-no-slashes")
	if err == nil {
		t.Error("NewNEGBindingTopologyProvider() with invalid defaultSubnetURL returned no error")
	}
}
