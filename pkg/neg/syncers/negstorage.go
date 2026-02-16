package syncers

import (
	nodetopologyv1 "github.com/GoogleCloudPlatform/gke-networking-api/apis/nodetopology/v1"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/neg/metrics"
	"k8s.io/ingress-gce/pkg/network"
	"k8s.io/klog/v2"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
)

type NEGNameWithZone struct {
	Name string
	Zone string
}

type NEGStorage interface {
	StartSync(needEnsureNegs bool) error
	GetSubnetNEGName(subnet string) (string, error)
	GetExistingZones() (sets.Set[string], error)
	GetExistingSubnets() (sets.Set[string], error)

	UpdateInitStatus(negObjs []*composite.NetworkEndpointGroup, errList []error)
	UpdateStatus(syncErr error) bool
	ComputeEPSStaleness([]*discovery.EndpointSlice)

	EnsureNeg(negName, zone string, networkInfo network.NetworkInfo) (*composite.NetworkEndpointGroup, error)
	GenerateSubnetToNegNameMap(subnetConfigs []nodetopologyv1.SubnetConfig) (map[string]string, error)
	ListEndpoints(negName, zone string, candidateZonesMap sets.Set[string], cloud negtypes.NetworkEndpointGroupCloud, version meta.Version, logger klog.Logger, negMetrics *metrics.NegMetrics) ([]*composite.NetworkEndpointWithHealthStatus, error)
	LimitTargetMapToExistingNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet
	ExcludeCleanedUpNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet

	StartNegCleanup(negName string)
	StopNegCleanup(negName string)
	ShouldKeepNegEmpty(negName string) bool
	CompleteNegCleanup(negName string)
}
