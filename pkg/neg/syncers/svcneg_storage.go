package syncers

import (
	"fmt"
	"time"

	"context"

	nodetopologyv1 "github.com/GoogleCloudPlatform/gke-networking-api/apis/nodetopology/v1"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	gcpmeta "github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	negv1beta1 "k8s.io/ingress-gce/pkg/apis/svcneg/v1beta1"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/flags"
	"k8s.io/ingress-gce/pkg/neg/metrics"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	"k8s.io/ingress-gce/pkg/network"
	svcnegclient "k8s.io/ingress-gce/pkg/svcneg/client/clientset/versioned"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/patch"
	"k8s.io/ingress-gce/pkg/utils/zonegetter"
	"k8s.io/klog/v2"
)

type SvcNEGStorage struct {
	kubeSystemUID string
	customName    bool
	namer         negtypes.NetworkEndpointGroupNamer
	negSyncerKey  negtypes.NegSyncerKey
	networkInfo   network.NetworkInfo
	zoneGetter    *zonegetter.ZoneGetter
	recorder      record.EventRecorder

	cloud      negtypes.NetworkEndpointGroupCloud
	negMetrics *metrics.NegMetrics
	logger     klog.Logger

	serviceLister cache.Indexer
	svcNegLister  cache.Indexer
	svcNegClient  svcnegclient.Interface
}

func NewSvcNegStorage(
	kubeSystemUID string,
	customName bool,
	namer negtypes.NetworkEndpointGroupNamer,
	negSyncerKey negtypes.NegSyncerKey,
	networkInfo network.NetworkInfo,
	zoneGetter *zonegetter.ZoneGetter,
	recorder record.EventRecorder,
	cloud negtypes.NetworkEndpointGroupCloud,
	negMetrics *metrics.NegMetrics,
	logger klog.Logger,
	serviceLister cache.Indexer,
	svcNegLister cache.Indexer,
	svcNegClient svcnegclient.Interface,
) NEGStorage {
	return &SvcNEGStorage{
		kubeSystemUID: kubeSystemUID,
		customName:    customName,
		namer:         namer,
		negSyncerKey:  negSyncerKey,
		networkInfo:   networkInfo,
		zoneGetter:    zoneGetter,
		recorder:      recorder,
		cloud:         cloud,
		negMetrics:    negMetrics,
		logger:        logger,
		serviceLister: serviceLister,
		svcNegLister:  svcNegLister,
		svcNegClient:  svcNegClient,
	}
}

func (s *SvcNEGStorage) StartSync(needEnsureNegs bool) error {
	return nil
}

func (s *SvcNEGStorage) GetSubnetNEGName(subnet string) (string, error) {
	defaultSubnet, err := utils.KeyName(s.networkInfo.SubnetworkURL)
	if err != nil {
		s.logger.Error(err, "Errored getting default subnet from NetworkInfo when retrieving existing endpoints")
		return "", err
	}

	if subnet == defaultSubnet {
		return s.negSyncerKey.NegName, nil
	}

	return s.GetNonDefaultSubnetNEGName(subnet)
}

func (s *SvcNEGStorage) GetNonDefaultSubnetNEGName(subnet string) (string, error) {
	if s.customName {
		negName, err := s.namer.NonDefaultSubnetCustomNEG(s.negSyncerKey.NegName, subnet)
		if err != nil {
			return "", err
		}
		return negName, nil
	}

	return s.namer.NonDefaultSubnetNEG(s.negSyncerKey.Namespace, s.negSyncerKey.Name, subnet, s.negSyncerKey.PortTuple.Port), nil
}

func (s *SvcNEGStorage) GetExistingZones() (sets.Set[string], error) {
	existingZones := sets.New[string]()
	negCR, err := s.GetSvcNEGFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve neg from the store", "neg", klog.KRef(s.negSyncerKey.Namespace, s.negSyncerKey.NegName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return existingZones, err
	}

	for _, ref := range negCR.Status.NetworkEndpointGroups {
		id, err := cloud.ParseResourceURL(ref.SelfLink)
		if err != nil {
			s.logger.Error(err, "unable to parse selflink", "selfLink", ref.SelfLink)
			s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
			continue
		}
		existingZones.Insert(id.Key.Zone)
	}
	return existingZones, nil
}

func (s *SvcNEGStorage) GetExistingSubnets() (sets.Set[string], error) {
	existingSubnets := sets.New[string]()
	negCR, err := s.GetSvcNEGFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve neg from the store", "neg", klog.KRef(s.negSyncerKey.Namespace, s.negSyncerKey.NegName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return existingSubnets, err
	}

	for _, ref := range negCR.Status.NetworkEndpointGroups {
		// If the subnet url is empty it means that the reference was created before
		// Subnets were populated by the controller. This is only possible with the subnetwork
		// that is specificed in networkInfo, and therefore we can assume which subnetwork was
		// used for this NEG
		subnetURL := s.networkInfo.SubnetworkURL
		if ref.SubnetURL != "" {
			subnetURL = ref.SubnetURL
		}
		id, err := cloud.ParseResourceURL(subnetURL)
		if err != nil {
			s.logger.Error(err, "unable to parse subnet url", "url", ref.SubnetURL)
			s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
			continue
		}

		existingSubnets.Insert(id.Key.Name)
	}
	return existingSubnets, nil
}

func (s *SvcNEGStorage) UpdateInitStatus(negObjs []*composite.NetworkEndpointGroup, errList []error) {
	if s.svcNegClient == nil {
		return
	}

	origNeg, err := s.GetSvcNEGFromStore()
	if err != nil {
		s.logger.Error(err, "Error updating init status for neg, failed to get neg from store.")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return
	}

	negRefs := make([]negv1beta1.NegObjectReference, len(negObjs))
	for i, obj := range negObjs {
		negRefs[i] = s.NegToNegRef(obj)
	}

	neg := origNeg.DeepCopy()
	if flags.F.EnableMultiSubnetClusterPhase1 {
		nonActiveNegRefs := getNonActiveNegRefs(origNeg.Status.NetworkEndpointGroups, negRefs, s.zoneGetter.ListSubnets(s.logger), s.networkInfo.SubnetworkURL, s.logger)
		negRefs = append(negRefs, nonActiveNegRefs...)
	}
	neg.Status.NetworkEndpointGroups = negRefs

	initializedCondition := s.getInitializedCondition(utilerrors.NewAggregate(errList))
	finalCondition := s.ensureCondition(neg, initializedCondition)
	s.negMetrics.PublishNegInitializationMetrics(finalCondition.LastTransitionTime.Sub(origNeg.GetCreationTimestamp().Time))

	_, err = s.patchNegStatus(origNeg.Status, neg.Status)
	if err != nil {
		s.logger.Error(err, "Error updating Neg CR")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
	}
}

func (s *SvcNEGStorage) UpdateStatus(syncErr error) bool {
	if s.svcNegClient == nil {
		return false
	}
	origNeg, err := s.GetSvcNEGFromStore()
	if err != nil {
		s.logger.Error(err, "Error updating status for neg, failed to get neg from store")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return false
	}
	neg := origNeg.DeepCopy()

	needInit := false
	ts := metav1.Now()
	if _, _, exists := s.findCondition(neg.Status.Conditions, negv1beta1.Initialized); !exists {
		needInit = true
	}
	s.negMetrics.PublishNegSyncerStalenessMetrics(ts.Sub(neg.Status.LastSyncTime.Time))

	s.ensureCondition(neg, getSyncedCondition(syncErr))
	neg.Status.LastSyncTime = ts

	if len(neg.Status.NetworkEndpointGroups) == 0 {
		needInit = true
	}

	_, err = s.patchNegStatus(origNeg.Status, neg.Status)
	if err != nil {
		s.logger.Error(err, "Error updating Neg CR")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
	}

	return needInit
}

func (s *SvcNEGStorage) ComputeEPSStaleness(endpointSlices []*discovery.EndpointSlice) {
	negCR, err := s.GetSvcNEGFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve neg from the store", "neg", klog.KRef(s.negSyncerKey.Namespace, s.negSyncerKey.NegName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return
	}
	lastSyncTimestamp := negCR.Status.LastSyncTime
	for _, endpointSlice := range endpointSlices {
		epsCreationTimestamp := endpointSlice.ObjectMeta.CreationTimestamp

		epsStaleness := time.Since(lastSyncTimestamp.Time)
		// if this endpoint slice is newly created/created after last sync
		if lastSyncTimestamp.Before(&epsCreationTimestamp) {
			epsStaleness = time.Since(epsCreationTimestamp.Time)
		}
		s.negMetrics.PublishNegEPSStalenessMetrics(epsStaleness)
		s.logger.V(3).Info("Endpoint slice syncs", "Namespace", endpointSlice.Namespace, "Name", endpointSlice.Name, "staleness", epsStaleness)
	}
}

func (s *SvcNEGStorage) EnsureNeg(negName, zone string, networkInfo network.NetworkInfo) (*composite.NetworkEndpointGroup, error) {
	return ensureNetworkEndpointGroup(
		s.negSyncerKey.Namespace,
		s.negSyncerKey.Name,
		negName,
		zone,
		s.negSyncerKey.String(),
		s.kubeSystemUID,
		fmt.Sprint(s.negSyncerKey.PortTuple.Port),
		s.negSyncerKey.NegType,
		s.cloud,
		s.serviceLister,
		s.recorder,
		s.negSyncerKey.GetAPIVersion(),
		s.customName,
		networkInfo,
		s.logger,
		s.negMetrics,
	)
}

func (s *SvcNEGStorage) GenerateSubnetToNegNameMap(subnetConfigs []nodetopologyv1.SubnetConfig) (map[string]string, error) {
	defaultSubnet, err := utils.KeyName(s.networkInfo.SubnetworkURL)
	if err != nil {
		s.logger.Error(err, "Errored getting default subnet from NetworkInfo when retrieving existing endpoints")
		return nil, err
	}

	subnetToNegMapping := make(map[string]string)
	// If networkInfo is not on the default subnet, then this service is using
	// multi-networking which cannot be used with multi subnet clusters. Even though
	// multi-networking subnet is using a non default subnet name, we use the default
	// neg naming which differs from how multi subnet cluster non default NEG names are
	// handled.
	if !s.networkInfo.IsDefault {
		subnetToNegMapping[defaultSubnet] = s.negSyncerKey.NegName
		return subnetToNegMapping, nil
	}

	for _, subnetConfig := range subnetConfigs {
		// negs in default subnet have a different naming scheme from other subnets
		if subnetConfig.Name == defaultSubnet {
			subnetToNegMapping[defaultSubnet] = s.negSyncerKey.NegName
			continue
		}
		nonDefaultNegName, err := s.GetNonDefaultSubnetNEGName(subnetConfig.Name)
		if err != nil {
			s.logger.Error(err, "Errored when getting NEG name from non-default subnets when retrieving existing endpoints")
			return nil, err
		}
		subnetToNegMapping[subnetConfig.Name] = nonDefaultNegName
	}

	return subnetToNegMapping, nil
}

func (s *SvcNEGStorage) ListEndpoints(negName, zone string, candidateZonesMap sets.Set[string], cloud negtypes.NetworkEndpointGroupCloud, version gcpmeta.Version, logger klog.Logger, negMetrics *metrics.NegMetrics) ([]*composite.NetworkEndpointWithHealthStatus, error) {
	networkEndpointsWithHealthStatus, err := cloud.ListNetworkEndpoints(negName, zone, false, version, logger)
	if err != nil {
		// It is possible for a NEG to be missing in a zone without candidate nodes. Log and ignore this error.
		// NEG not found in a candidate zone is an error.
		if utils.IsNotFoundError(err) && !candidateZonesMap.Has(zone) {
			logger.Info("Ignoring NotFound error for NEG", "negName", negName, "zone", zone)
			negMetrics.PublishNegControllerErrorCountMetrics(err, true)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to lookup NEG in zone %q, candidate zones %v, err - %w", zone, candidateZonesMap, err)
	}
	return networkEndpointsWithHealthStatus, nil
}

func (s *SvcNEGStorage) LimitTargetMapToExistingNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet {
	return targetMap
}

func (s *SvcNEGStorage) ExcludeCleanedUpNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet {
	return targetMap
}

func (s *SvcNEGStorage) StartNegCleanup(negName string) {}

func (s *SvcNEGStorage) StopNegCleanup(negName string) {}

func (s *SvcNEGStorage) ShouldKeepNegEmpty(negName string) bool {
	return false
}

func (s *SvcNEGStorage) CompleteNegCleanup(negName string) {}

// GetSvcNEGFromStore returns the neg associated with the provided namespace and neg name if it exists otherwise throws an error
func (s *SvcNEGStorage) GetSvcNEGFromStore() (*negv1beta1.ServiceNetworkEndpointGroup, error) {
	n, exists, err := s.svcNegLister.GetByKey(fmt.Sprintf("%s/%s", s.negSyncerKey.Namespace, s.negSyncerKey.NegName))
	if err != nil {
		return nil, fmt.Errorf("error getting neg %s/%s from cache: %w", s.negSyncerKey.Namespace, s.negSyncerKey.NegName, err)
	}
	if !exists {
		return nil, fmt.Errorf("neg %s/%s is not in store", s.negSyncerKey.Namespace, s.negSyncerKey.NegName)
	}

	return n.(*negv1beta1.ServiceNetworkEndpointGroup), nil
}

func (s *SvcNEGStorage) NegToNegRef(neg *composite.NetworkEndpointGroup) negv1beta1.NegObjectReference {
	negRef := negv1beta1.NegObjectReference{
		Id:                  fmt.Sprint(neg.Id),
		SelfLink:            neg.SelfLink,
		NetworkEndpointType: negv1beta1.NetworkEndpointType(neg.NetworkEndpointType),
	}
	if flags.F.EnableMultiSubnetClusterPhase1 {
		negRef.State = negv1beta1.ActiveState
		negRef.SubnetURL = neg.Subnetwork
	}
	return negRef
}

// patchNegStatus patches the specified NegCR status with the provided new status
func (s *SvcNEGStorage) patchNegStatus(oldStatus, newStatus negv1beta1.ServiceNetworkEndpointGroupStatus) (*negv1beta1.ServiceNetworkEndpointGroup, error) {
	patchBytes, err := patch.MergePatchBytes(negv1beta1.ServiceNetworkEndpointGroup{Status: oldStatus}, negv1beta1.ServiceNetworkEndpointGroup{Status: newStatus})
	if err != nil {
		return nil, fmt.Errorf("failed to prepare patch bytes: %w", err)
	}

	start := time.Now()
	neg, err := s.svcNegClient.NetworkingV1beta1().ServiceNetworkEndpointGroups(s.negSyncerKey.Namespace).Patch(context.Background(), s.negSyncerKey.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	s.negMetrics.PublishK8sRequestCountMetrics(start, metrics.DeleteRequest, err)
	return neg, err
}

// findCondition finds a condition in the given list of conditions that has the type conditionType and returns the condition and its index.
// If no condition is found, an empty condition, -1 and false will be returned to indicate the condition does not exist.
func (s *SvcNEGStorage) findCondition(conditions []negv1beta1.Condition, conditionType string) (negv1beta1.Condition, int, bool) {
	for i, c := range conditions {
		if c.Type == conditionType {
			return c, i, true
		}
	}

	return negv1beta1.Condition{}, -1, false
}

func (s *SvcNEGStorage) getInitializedCondition(err error) negv1beta1.Condition {
	if err != nil {
		return negv1beta1.Condition{
			Type:               negv1beta1.Initialized,
			Status:             v1.ConditionFalse,
			Reason:             negtypes.NegInitializationFailed,
			LastTransitionTime: metav1.Now(),
			Message:            err.Error(),
		}
	}

	return negv1beta1.Condition{
		Type:               negv1beta1.Initialized,
		Status:             v1.ConditionTrue,
		Reason:             negtypes.NegInitializationSuccessful,
		LastTransitionTime: metav1.Now(),
	}
}

// ensureCondition will update the condition on the neg object if necessary
func (s *SvcNEGStorage) ensureCondition(neg *negv1beta1.ServiceNetworkEndpointGroup, expectedCondition negv1beta1.Condition) negv1beta1.Condition {
	condition, index, exists := s.findCondition(neg.Status.Conditions, expectedCondition.Type)
	if !exists {
		neg.Status.Conditions = append(neg.Status.Conditions, expectedCondition)
		return expectedCondition
	}

	if condition.Status == expectedCondition.Status {
		expectedCondition.LastTransitionTime = condition.LastTransitionTime
	}

	neg.Status.Conditions[index] = expectedCondition
	return expectedCondition
}
