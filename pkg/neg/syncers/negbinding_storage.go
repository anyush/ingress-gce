package syncers

import (
	"fmt"
	"sync"
	"time"

	"context"

	nodetopologyv1 "github.com/GoogleCloudPlatform/gke-networking-api/apis/nodetopology/v1"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	gcpmeta "github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	negbindingv1beta1 "k8s.io/ingress-gce/pkg/apis/negbinding/v1beta1"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/neg/metrics"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	negbindingclient "k8s.io/ingress-gce/pkg/negbinding/client/clientset/versioned"
	"k8s.io/ingress-gce/pkg/network"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/klog/v2"
)

const (
	Managed            = "Managed"
	BackendRefAttached = "BackendRefAttached"
	NegsAttached       = "NegsAttached"
)

type NegId struct {
	Name   string
	Subnet string
	Zone   string
}

type boundNegErrorType string

const (
	negNotSpecifiedForSubnet = boundNegErrorType("neg-not-specified-for-subnet")
	negNotFoundInZone        = boundNegErrorType("neg-not-found-in-zone")
	negInvalidDescription    = boundNegErrorType("neg-invalid-description")
	podInLocationWithoutNeg  = boundNegErrorType("pod-in-location-without-neg")
)

type boundNegError struct {
	Type    boundNegErrorType
	Message string
	Name    string
	Zone    string
	Subnet  string
}

func (err boundNegError) Error() string {
	switch err.Type {
	case negNotSpecifiedForSubnet:
		return fmt.Sprintf("Neg name for %s subnet not specified", err.Subnet)
	case negNotFoundInZone:
		return fmt.Sprintf("Neg %s not found in %s zone", err.Name, err.Zone)
	case negInvalidDescription:
		return fmt.Sprintf("description of Neg %s in zone %s is invalid: %s", err.Name, err.Zone, err.Message)
	case podInLocationWithoutNeg:
		return fmt.Sprintf("pod found in location where is no valid Neg (subnet: %s, zone: %s)", err.Subnet, err.Zone)
	default:
		return fmt.Sprintf("unknown Neg error %v", err)
	}
}

type NegCleanupStatus string

const (
	NegCleanupInProgress = NegCleanupStatus("neg-cleanup-in-progress")
	NegCleanupCompleted  = NegCleanupStatus("neg-cleanup-completed")
)

type NEGBindingStorage struct {
	namer       namer.NonDefaultSubnetNEGNamer
	logger      klog.Logger
	networkInfo network.NetworkInfo
	apiVersion  gcpmeta.Version
	cloud       negtypes.NetworkEndpointGroupCloud
	negMetrics  *metrics.NegMetrics

	negBindingLister cache.Indexer
	negBindingClient negbindingclient.Interface

	negBindingName string
	namespace      string

	ensuredNegs         sets.Set[NegId]
	boundNegErrors      []boundNegError
	cleanupLock         sync.Mutex
	negCleanupStatusMap map[string]NegCleanupStatus
	onCleanupCompleted  func()
}

func NewNegBindingStorage(
	namer namer.NonDefaultSubnetNEGNamer,
	logger klog.Logger,
	networkInfo network.NetworkInfo,
	apiVersion gcpmeta.Version,
	cloud negtypes.NetworkEndpointGroupCloud,
	negMetrics *metrics.NegMetrics,
	negBindingLister cache.Indexer,
	negBindingClient negbindingclient.Interface,
	negBindingName string,
	namespace string,
	onCleanupCompleted func(),
) NEGStorage {
	return &NEGBindingStorage{
		namer:               namer,
		logger:              logger,
		networkInfo:         networkInfo,
		apiVersion:          apiVersion,
		cloud:               cloud,
		negMetrics:          negMetrics,
		negBindingLister:    negBindingLister,
		negBindingClient:    negBindingClient,
		negBindingName:      negBindingName,
		namespace:           namespace,
		ensuredNegs:         sets.New[NegId](),
		boundNegErrors:      []boundNegError{},
		negCleanupStatusMap: map[string]NegCleanupStatus{},
		onCleanupCompleted:  onCleanupCompleted,
	}
}

func (s *NEGBindingStorage) StartSync(needEnsureNegs bool) error {
	if needEnsureNegs {
		s.ensuredNegs = sets.New[NegId]()
	}
	return nil
}

func (s *NEGBindingStorage) GetSubnetNEGName(subnet string) (string, error) {
	negName := s.namer.NonDefaultSubnetNEG("", "", subnet, 0)
	if negName == "" {
		err := boundNegError{
			Type:   negNotSpecifiedForSubnet,
			Subnet: subnet,
		}
		s.ensureBoundNegError(err)
		return "", fmt.Errorf("no Neg specified on NegBinding for subnet %s", subnet)
	}
	return negName, nil
}

func (s *NEGBindingStorage) GetExistingZones() (sets.Set[string], error) {
	existingZones := sets.New[string]()
	negBinding, err := s.GetNegBindingFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve NEGBinding from the store", "NEGBinding", klog.KRef(s.namespace, s.negBindingName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return existingZones, err
	}
	for _, ref := range negBinding.Status.NetworkEndpointGroups {
		id, err := cloud.ParseResourceURL(ref.ResourceURL)
		if err != nil {
			s.logger.Error(err, "unable to parse selflink", "selfLink", ref.ResourceURL)
			s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
			continue
		}
		existingZones.Insert(id.Key.Zone)
	}
	return existingZones, nil
}

func (s *NEGBindingStorage) GetExistingSubnets() (sets.Set[string], error) {
	existingSubnets := sets.New[string]()
	negBinding, err := s.GetNegBindingFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve NEGBinding from the store", "NEGBinding", klog.KRef(s.namespace, s.negBindingName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return existingSubnets, err
	}

	for _, ref := range negBinding.Status.NetworkEndpointGroups {
		id, err := cloud.ParseResourceURL(ref.SubnetURL)
		if err != nil {
			s.logger.Error(err, "unable to parse subnet url", "url", ref.SubnetURL)
			s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
			continue
		}

		existingSubnets.Insert(id.Key.Name)
	}
	return existingSubnets, nil
}

func (s *NEGBindingStorage) UpdateInitStatus(negObjs []*composite.NetworkEndpointGroup, errList []error) {
	nb, err := s.GetNegBindingFromStore()
	if err != nil {
		s.logger.Error(err, "Error updating init status for NEGBinding, failed to get NEGBinding from store.")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return
	}

	negRefs := make([]negbindingv1beta1.StatusNegRef, len(negObjs))
	for i, obj := range negObjs {
		negRefs[i] = s.NegToNegRef(obj)
	}
	nb.Status.NetworkEndpointGroups = negRefs

	negsAttachedCondition := s.getNegsAttachedCondition()
	finalCondition := s.ensureCondition(nb, negsAttachedCondition)
	s.negMetrics.PublishNegInitializationMetrics(finalCondition.LastTransitionTime.Sub(nb.GetCreationTimestamp().Time))

	start := time.Now()
	_, err = s.negBindingClient.NetworkingV1beta1().NetworkEndpointGroupBindings(s.namespace).UpdateStatus(context.Background(), nb, metav1.UpdateOptions{})
	s.negMetrics.PublishK8sRequestCountMetrics(start, metrics.DeleteRequest, err)
	if err != nil {
		s.logger.Error(err, "Error updating NegBinding CR")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
	}
}

func (s *NEGBindingStorage) UpdateStatus(err error) bool {
	nb, err := s.GetNegBindingFromStore()
	if err != nil {
		s.logger.Error(err, "Error updating status for NegBinding, failed to get NegBinding from store")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return false
	}

	needInit := false
	ts := metav1.Now()
	_, _, exists := s.findCondition(nb.Status.Conditions, NegsAttached)
	if !exists {
		needInit = true
	}
	negsAttachedCondition := s.getNegsAttachedCondition()
	s.ensureCondition(nb, negsAttachedCondition)
	s.negMetrics.PublishNegSyncerStalenessMetrics(ts.Sub(nb.Status.LastSyncTime.Time))

	nb.Status.LastSyncTime = ts
	if len(nb.Status.NetworkEndpointGroups) == 0 {
		needInit = true
	}

	start := time.Now()
	_, err = s.negBindingClient.NetworkingV1beta1().NetworkEndpointGroupBindings(s.namespace).UpdateStatus(context.Background(), nb, metav1.UpdateOptions{})
	s.negMetrics.PublishK8sRequestCountMetrics(start, metrics.DeleteRequest, err)
	if err != nil {
		s.logger.Error(err, "Error updating NegBinding CR")
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
	}
	return needInit
}

func (s *NEGBindingStorage) ComputeEPSStaleness(endpointSlices []*discovery.EndpointSlice) {
	nb, err := s.GetNegBindingFromStore()
	if err != nil {
		s.logger.Error(err, "unable to retrieve NegBinding from the store", "NegBinding", klog.KRef(s.namespace, s.negBindingName))
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return
	}

	lastSyncTimestamp := nb.Status.LastSyncTime
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

func (s *NEGBindingStorage) EnsureNeg(negName, zone string, networkInfo network.NetworkInfo) (*composite.NetworkEndpointGroup, error) {
	negLogger := s.logger.WithValues("negName", negName, "zone", zone)
	negLogger.V(3).Info("EnsureNeg started")
	subnet, err := utils.KeyName(s.networkInfo.SubnetworkURL)
	if err != nil {
		s.logger.Error(err, "Failed to parse subnetURL", "subnetURL", s.networkInfo.SubnetworkURL)
		return nil, err
	}
	neg, err := s.cloud.GetNetworkEndpointGroup(negName, zone, s.apiVersion, s.logger)
	if err != nil {
		if !utils.IsNotFoundError(err) {
			negLogger.Error(err, "Failed to get Neg")
			return nil, err
		}
		negLogger.Info("Neg was not found", "err", err)
		s.negMetrics.PublishNegControllerErrorCountMetrics(err, true)

		boundNegErr := boundNegError{
			Type:   negNotFoundInZone,
			Name:   negName,
			Zone:   zone,
			Subnet: subnet,
		}
		s.ensureBoundNegError(boundNegErr)
		return nil, nil
	}

	s.ensuredNegs.Insert(
		NegId{
			Name:   negName,
			Subnet: subnet,
			Zone:   zone,
		},
	)
	negLogger.V(3).Info("EnsureNeg successfully completed", "ensuredNegs", s.ensuredNegs.UnsortedList())
	return neg, nil
}

func (s *NEGBindingStorage) GenerateSubnetToNegNameMap(subnetConfigs []nodetopologyv1.SubnetConfig) (map[string]string, error) {
	subnetToNegMapping := make(map[string]string)
	for _, subnetConfig := range subnetConfigs {
		negName := s.namer.NonDefaultSubnetNEG("", "", subnetConfig.Name, 0)
		if negName == "" {
			err := boundNegError{
				Type:   negNotSpecifiedForSubnet,
				Subnet: subnetConfig.Name,
			}
			s.ensureBoundNegError(err)
			continue
		}

		subnetToNegMapping[subnetConfig.Name] = negName
	}

	return subnetToNegMapping, nil
}

func (s *NEGBindingStorage) ListEndpoints(negName, zone string, candidateZonesMap sets.Set[string], cloud negtypes.NetworkEndpointGroupCloud, version gcpmeta.Version, logger klog.Logger, negMetrics *metrics.NegMetrics) ([]*composite.NetworkEndpointWithHealthStatus, error) {
	var neg *NegId
	for id := range s.ensuredNegs {
		if id.Name == negName && id.Zone == zone {
			neg = &id
		}
	}
	if neg == nil {
		return nil, nil
	}

	networkEndpointsWithHealthStatus, err := cloud.ListNetworkEndpoints(negName, zone, false, version, logger)
	if err != nil {
		if utils.IsNotFoundError(err) {
			negErr := boundNegError{
				Type:   negNotFoundInZone,
				Name:   neg.Name,
				Subnet: neg.Subnet,
				Zone:   neg.Zone,
			}
			s.ensuredNegs.Delete(*neg)
			s.ensureBoundNegError(negErr)
			return nil, nil
		}

		return nil, fmt.Errorf("failed to lookup NEG in zone %q, candidate zones %v, err - %w", zone, candidateZonesMap, err)
	}
	return networkEndpointsWithHealthStatus, nil
}

func (s *NEGBindingStorage) LimitTargetMapToExistingNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet {
	if targetMap == nil {
		return targetMap
	}

	s.logger.V(3).Info("LimitTargetMapToExistingNegs", "ensuredNegs", s.ensuredNegs.UnsortedList(), "cleanupStatusMap", s.negCleanupStatusMap)

	newTargetMap := make(map[negtypes.NEGLocation]negtypes.NetworkEndpointSet, len(targetMap))
	for neg := range s.ensuredNegs {
		negLocation := negtypes.NEGLocation{
			Subnet: neg.Subnet,
			Zone:   neg.Zone,
		}
		if eps, ok := targetMap[negLocation]; ok {
			newTargetMap[negLocation] = eps
		}
	}

	for location := range targetMap {
		if eps, ok := newTargetMap[location]; !ok && eps.Len() > 0 {
			err := boundNegError{
				Type:   podInLocationWithoutNeg,
				Subnet: location.Subnet,
				Zone:   location.Zone,
			}
			s.ensureBoundNegError(err)
		}
	}

	return newTargetMap
}

func (s *NEGBindingStorage) ExcludeCleanedUpNegs(targetMap map[negtypes.NEGLocation]negtypes.NetworkEndpointSet) map[negtypes.NEGLocation]negtypes.NetworkEndpointSet {
	if targetMap == nil {
		return targetMap
	}

	newTargetMap := make(map[negtypes.NEGLocation]negtypes.NetworkEndpointSet, len(targetMap))
	for location, eps := range targetMap {
		negName := s.namer.NonDefaultSubnetNEG("", "", location.Subnet, 0)
		if negName == "" {
			continue
		}

		if s.ShouldKeepNegEmpty(negName) {
			eps = negtypes.NewNetworkEndpointSet()
		}
		newTargetMap[location] = eps
	}
	return newTargetMap
}

func (s *NEGBindingStorage) StartNegCleanup(negName string) {
	s.cleanupLock.Lock()
	defer s.cleanupLock.Unlock()

	currentStatus, ok := s.negCleanupStatusMap[negName]
	if ok && currentStatus == NegCleanupCompleted {
		return
	}
	s.negCleanupStatusMap[negName] = NegCleanupInProgress
}

func (s *NEGBindingStorage) StopNegCleanup(negName string) {
	s.cleanupLock.Lock()
	defer s.cleanupLock.Unlock()

	if _, ok := s.negCleanupStatusMap[negName]; ok {
		delete(s.negCleanupStatusMap, negName)
	}
}

func (s *NEGBindingStorage) ShouldKeepNegEmpty(negName string) bool {
	s.cleanupLock.Lock()
	defer s.cleanupLock.Unlock()

	_, ok := s.negCleanupStatusMap[negName]
	return ok
}

func (s *NEGBindingStorage) CompleteNegCleanup(negName string) {
	s.cleanupLock.Lock()
	defer s.cleanupLock.Unlock()

	if _, ok := s.negCleanupStatusMap[negName]; !ok {
		return
	}
	s.negCleanupStatusMap[negName] = NegCleanupCompleted

	if s.onCleanupCompleted == nil {
		return
	}
	for _, status := range s.negCleanupStatusMap {
		if status != NegCleanupCompleted {
			return
		}
	}
	s.onCleanupCompleted()
	// s.negCleanupStatusMap = make(map[string]NegCleanupStatus)
}

// getNegBindingFromStore returns the NEGBinding associated with the provided namespace and name if it exists otherwise throws an error
func (s *NEGBindingStorage) GetNegBindingFromStore() (*negbindingv1beta1.NetworkEndpointGroupBinding, error) {
	nb, exists, err := s.negBindingLister.GetByKey(fmt.Sprintf("%s/%s", s.namespace, s.negBindingName))
	if err != nil {
		return nil, fmt.Errorf("error getting NEGBinding %s/%s from cache: %w", s.namespace, s.negBindingName, err)
	}
	if !exists {
		return nil, fmt.Errorf("NEGBinding %s/%s is not in store", s.namespace, s.negBindingName)
	}
	return nb.(*negbindingv1beta1.NetworkEndpointGroupBinding), nil
}

func (s *NEGBindingStorage) NegToNegRef(neg *composite.NetworkEndpointGroup) negbindingv1beta1.StatusNegRef {
	s.cleanupLock.Lock()
	defer s.cleanupLock.Unlock()

	// state := negbindingv1beta1.NegStateActive
	// if cleanupStatus, ok := s.negCleanupStatusMap[neg.Name]; ok {
	// 	switch cleanupStatus {
	// 	case NegCleanupInProgress:
	// 		state = negbindingv1beta1.NegStateCleaningUp
	// 	case NegCleanupCompleted:
	// 		state = negbindingv1beta1.NegStateCleanedUp
	// 	}
	// }

	return negbindingv1beta1.StatusNegRef{
		ResourceURL: neg.SelfLink,
		SubnetURL:   neg.Subnetwork,
		// State:       state,
	}
}

func (s *NEGBindingStorage) getNegsAttachedCondition() negbindingv1beta1.Condition {
	if len(s.boundNegErrors) > 0 {
		errs := make([]error, len(s.boundNegErrors))
		for _, err := range s.boundNegErrors {
			errs = append(errs, err)
		}

		return negbindingv1beta1.Condition{
			Type:               NegsAttached,
			Status:             metav1.ConditionFalse,
			Reason:             negtypes.NegInitializationFailed,
			LastTransitionTime: metav1.Now(),
			Message:            utilerrors.NewAggregate(errs).Error(),
		}
	}

	return negbindingv1beta1.Condition{
		Type:               NegsAttached,
		Status:             metav1.ConditionTrue,
		Reason:             negtypes.NegInitializationSuccessful,
		LastTransitionTime: metav1.Now(),
	}
}

func (s *NEGBindingStorage) ensureCondition(negBinding *negbindingv1beta1.NetworkEndpointGroupBinding, expectedCondition negbindingv1beta1.Condition) negbindingv1beta1.Condition {
	condition, index, exists := s.findCondition(negBinding.Status.Conditions, expectedCondition.Type)
	if !exists {
		negBinding.Status.Conditions = append(negBinding.Status.Conditions, expectedCondition)
		return expectedCondition
	}

	if condition.Status == expectedCondition.Status {
		expectedCondition.LastTransitionTime = condition.LastTransitionTime
	}

	negBinding.Status.Conditions[index] = expectedCondition
	return expectedCondition
}

// findCondition finds a condition in the given list of conditions that has the type conditionType and returns the condition and its index.
// If no condition is found, an empty condition, -1 and false will be returned to indicate the condition does not exist.
func (s *NEGBindingStorage) findCondition(conditions []negbindingv1beta1.Condition, conditionType string) (negbindingv1beta1.Condition, int, bool) {
	for i, c := range conditions {
		if c.Type == conditionType {
			return c, i, true
		}
	}

	return negbindingv1beta1.Condition{}, -1, false
}

func (s *NEGBindingStorage) ensureBoundNegError(err boundNegError) {
	for _, e := range s.boundNegErrors {
		if e == err {
			return
		}
	}
	s.boundNegErrors = append(s.boundNegErrors, err)
}
