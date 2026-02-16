package neg

import (
	"fmt"
	"sync"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	negbindingv1beta1 "k8s.io/ingress-gce/pkg/apis/negbinding/v1beta1"
	negsyncer "k8s.io/ingress-gce/pkg/neg/syncers"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	"k8s.io/ingress-gce/pkg/network"
	"k8s.io/klog/v2"
)

type NegBindingSyncerManager struct {
	baseManager *syncerManager
	mu          sync.Mutex
	// syncerMap   map[NegBindingRef]negtypes.NegSyncer
	// negStorageMap map[NegBindingRef]*negsyncer.NEGBindingStorage
	// bindingToNegMap map[NegBindingRef]sets.Set[string]

	syncerStateMap map[NegBindingRef]*SyncerState
}

type SyncerState struct {
	syncer       negtypes.NegSyncer
	negStorage   negsyncer.NEGStorage
	assignedNegs sets.Set[string]
	serviceKey   serviceKey
}

// type NegBindingWithIgnoredNegs struct {
// 	*negbindingv1beta1.NetworkEndpointGroupBinding
// 	IgnoredNegs sets.Set[string]
// }

type NegBindingRef struct {
	Name      string
	Namespace string
}

func (ref NegBindingRef) String() string {
	return fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)
}

// type NegSyncInfo struct {
// 	Service serviceKey
// 	Binding NegBindingRef
// 	Subnet string
// 	Port int32
// }

// type NegSyncInfoMap map[string]NegSyncInfo

// func (m NegSyncInfoMap) Get(key string) (NegSyncInfo, bool) {
// 	info, ok := m[key]
// 	return info, ok
// }

// func (m NegSyncInfoMap) Has(key string) bool {
// 	_, ok := m[key]
// 	return ok
// }

// func (m NegSyncInfoMap) Insert(key string, info NegSyncInfo) {
// 	m[key] = info
// }

type NoopNegNamer struct {
	negNameMap map[string]string
}

func NewNoopNegNamer(subnetToNegNameMap map[string]string) NoopNegNamer {
	return NoopNegNamer{negNameMap: subnetToNegNameMap}
}

func (n NoopNegNamer) NonDefaultSubnetNEG(namespace, name, subnetName string, port int32) string {
	return n.negNameMap[subnetName]
}

func (n NoopNegNamer) NonDefaultSubnetCustomNEG(customNEGName, subnetName string) (string, error) {
	return n.negNameMap[subnetName], nil
}

func NewNegBindingSyncerManager(baseManager *syncerManager) *NegBindingSyncerManager {
	m := &NegBindingSyncerManager{baseManager: baseManager}
	m.loadStatusFromCRs()
	return m
}

func (m *NegBindingSyncerManager) loadStatusFromCRs() {
	m.syncerStateMap = make(map[NegBindingRef]*SyncerState)
	bindingIs := m.baseManager.negBindingLister.List()

	for _, bindingI := range bindingIs {
		binding, ok := bindingI.(*negbindingv1beta1.NetworkEndpointGroupBinding)
		if !ok {
			continue
		}
		bindingRef := m.bindingToBindingRef(binding)

		negNames := sets.New[string]()
		for _, negStatusRef := range binding.Status.NetworkEndpointGroups {
			negRef, err := m.statusToSpecNegRef(negStatusRef)
			if err != nil {
				continue
			}
			negNames.Insert(negRef.Name)
		}

		m.syncerStateMap[bindingRef] = &SyncerState{
			assignedNegs: negNames,
			serviceKey:   getServiceKey(binding.Namespace, binding.Spec.BackendRef.Name),
		}
	}
}

func (m *NegBindingSyncerManager) EnsureSyncerForNegBinding(binding *negbindingv1beta1.NetworkEndpointGroupBinding, networkInfo *network.NetworkInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	svcKey := binding.Namespace + "/" + binding.Spec.BackendRef.Name
	obj, exists, err := m.baseManager.serviceLister.GetByKey(svcKey)
	if err != nil {
		return err
		// TODO(yushkevicha) process error
	}
	if !exists {
		return fmt.Errorf("svc does not exist")
		// TODO(yushkevicha) process 404, stop syncer as well if exists
	}

	svc := obj.(*apiv1.Service)

	syncerState := m.ensureValidSyncerState(binding)
	currentNegs := syncerState.assignedNegs
	if currentNegs == nil {
		currentNegs = sets.New[string]()
	}
	specifiedNegs, assignedNegs := m.getSpecifiedAndAssignedNegs(binding, syncerState)
	newNegs := assignedNegs.Difference(currentNegs)
	removedNegs := currentNegs.Difference(specifiedNegs)

	m.baseManager.logger.V(3).Info("Neg sets for binding", "bindingName", binding.Name, "current", currentNegs.UnsortedList(), "specified", specifiedNegs.UnsortedList(), "assigned", assignedNegs.UnsortedList(), "new", newNegs.UnsortedList(), "removed", removedNegs.UnsortedList())

	m.baseManager.logger.V(3).Info("NegBinding processing started", "bindingName", binding.Name, "syncerExists", syncerState.syncer != nil, "syncer", syncerState.syncer, "newNegs", newNegs.UnsortedList(), "removedNegs", removedNegs.UnsortedList())
	if syncerState.syncer != nil && newNegs.Len() == 0 && removedNegs.Len() == 0 {
		// No changes should be performed
		return nil
	}

	if syncerState.syncer != nil && newNegs.Len() != 0 {
		syncerState.syncer.Stop()
		syncerState = m.ensureValidSyncerState(binding)
	}

	if syncerState.syncer == nil {
		syncerKey, err := m.getSyncerKey(binding, svc)
		if err != nil {
			// TODO(yushkevicha)
			return err
		}

		epc := negsyncer.GetEndpointsCalculator(
			m.baseManager.podLister,
			m.baseManager.nodeLister,
			m.baseManager.serviceLister,
			m.baseManager.zoneGetter,
			syncerKey,
			negtypes.L7Mode,
			m.baseManager.logger.WithValues("service", klog.KRef(svc.Namespace, svc.Name), "negBinding", klog.KRef(binding.Namespace, binding.Name)),
			m.baseManager.enableDualStackNEG,
			m.baseManager.syncerMetrics,
			networkInfo,
			"",
			m.baseManager.negMetrics,
		)

		subnetToNegMap := m.getSubnetToNegMap(binding, assignedNegs)
		namer := NewNoopNegNamer(subnetToNegMap)

		syncerState.negStorage = negsyncer.NewNegBindingStorage(
			namer,
			m.baseManager.logger,
			*networkInfo,
			syncerKey.GetAPIVersion(),
			m.baseManager.cloud,
			m.baseManager.negMetrics,
			m.baseManager.negBindingLister,
			m.baseManager.negBindingClient,
			syncerKey.NegBindingName,
			syncerKey.Namespace,
			func() {},
			// func() { go m.EnsureSyncersForService(svc, networkInfo) },
		)

		syncerState.syncer = negsyncer.NewTransactionSyncer(
			syncerKey,
			m.baseManager.recorder,
			m.baseManager.cloud,
			m.baseManager.zoneGetter,
			m.baseManager.podLister,
			m.baseManager.serviceLister,
			m.baseManager.endpointSliceLister,
			m.baseManager.reflector,
			epc,
			string(m.baseManager.kubeSystemUID),
			m.baseManager.syncerMetrics,
			m.baseManager.logger,
			m.baseManager.lpConfig,
			m.baseManager.enableDualStackNEG,
			*networkInfo,
			m.baseManager.negMetrics,
			syncerState.negStorage,
		)
	}

	for negName := range specifiedNegs {
		syncerState.negStorage.StopNegCleanup(negName)
	}

	if removedNegs.Len() != 0 {
		for negName := range removedNegs {
			syncerState.negStorage.StartNegCleanup(negName)
		}
	}

	if syncerState.syncer.IsStopped() {
		if err := syncerState.syncer.Start(); err != nil {
			// TODO(yushkevicha) process error
		}
	}

	syncerState.assignedNegs = assignedNegs
	m.baseManager.logger.V(3).Info("NegBinding processing completed", "bindingName", binding.Name, "finalState", *syncerState)
	return nil
}

func (m *NegBindingSyncerManager) ensureValidSyncerState(binding *negbindingv1beta1.NetworkEndpointGroupBinding) *SyncerState {
	bindingRef := m.bindingToBindingRef(binding)
	state, ok := m.syncerStateMap[bindingRef]
	if !ok {
		m.baseManager.logger.V(3).Info("Creating new state for NegBinding", "bindingName", binding.Name)
		state = &SyncerState{
			assignedNegs: sets.New[string](),
			serviceKey:   getServiceKey(binding.Namespace, binding.Spec.BackendRef.Name),
		}
	}

	if state.syncer != nil && state.syncer.IsStopped() {
		state.syncer = nil
	}
	if state.syncer != nil && state.negStorage == nil {
		state.syncer.Stop()
		state.syncer = nil
	}
	if state.syncer == nil {
		state.negStorage = nil
	}
	if state.assignedNegs == nil {
		state.assignedNegs = sets.New[string]()
	}

	m.syncerStateMap[bindingRef] = state
	return state
}

func (m *NegBindingSyncerManager) getSpecifiedAndAssignedNegs(binding *negbindingv1beta1.NetworkEndpointGroupBinding, syncerState *SyncerState) (sets.Set[string], sets.Set[string]) {
	specifiedNegs := sets.New[string]()
	assignedNegs := sets.New[string]()
	subnetsCovered := sets.New[string]()

	negsManagedByOtherBindings := sets.New[string]()
	bindingRef := m.bindingToBindingRef(binding)
	for bRef, state := range m.syncerStateMap {
		if bRef != bindingRef {
			negsManagedByOtherBindings = negsManagedByOtherBindings.Union(state.assignedNegs)
		}
	}

	statusNegs := sets.New[string]()
	for _, statusNegRef := range binding.Status.NetworkEndpointGroups {
		negRef, err := m.statusToSpecNegRef(statusNegRef)
		if err != nil {
			continue
		}

		statusNegs.Insert(negRef.Name)
		if !negsManagedByOtherBindings.Has(negRef.Name) {
			assignedNegs.Insert(negRef.Name)
			subnetsCovered.Insert(negRef.Subnet)
		}
	}

	// for neg := range syncerState.assignedNegs {
	// 	if !statusNegs.Has(neg) {

	// 	}
	// }

	for _, negRef := range binding.Spec.NetworkEndpointGroups {
		specifiedNegs.Insert(negRef.Name)
		if subnetsCovered.Has(negRef.Subnet) {
			continue
		}

		alreadyManaged := false
		for _, state := range m.syncerStateMap {
			if state.assignedNegs.Has(negRef.Name) {
				alreadyManaged = true
				break
			}
		}
		if !alreadyManaged {
			assignedNegs.Insert(negRef.Name)
			subnetsCovered.Insert(negRef.Subnet)
		}
	}

	return specifiedNegs, assignedNegs
}

func (m *NegBindingSyncerManager) getSubnetToNegMap(binding *negbindingv1beta1.NetworkEndpointGroupBinding, negsToInclude sets.Set[string]) map[string]string {
	subnetToNegMap := make(map[string]string)
	for _, negRef := range binding.Spec.NetworkEndpointGroups {
		if negsToInclude.Has(negRef.Name) {
			subnetToNegMap[negRef.Subnet] = negRef.Name
		}
	}

	for _, negStatusRef := range binding.Status.NetworkEndpointGroups {
		negRef, err := m.statusToSpecNegRef(negStatusRef)
		if err == nil && negsToInclude.Has(negRef.Name) {
			subnetToNegMap[negRef.Subnet] = negRef.Name
		}
	}
	return subnetToNegMap
}

func (m *NegBindingSyncerManager) EnsureSyncersForService(service *apiv1.Service, networkInfo *network.NetworkInfo) error {
	indexKey := service.Namespace + "/" + service.Name
	// TODO(yushkevicha): remove "magic value"
	bindingIs, err := m.baseManager.negBindingLister.ByIndex("service-index", indexKey)
	if err != nil {
		return fmt.Errorf("can't list NegBindings related to service %s: %v")
	}
	if len(bindingIs) == 0 {
		return nil
	}

	for _, bindingI := range bindingIs {
		binding, ok := bindingI.(*negbindingv1beta1.NetworkEndpointGroupBinding)
		if ok {
			m.EnsureSyncerForNegBinding(binding, networkInfo)
		}
	}
	return nil
}

// StopSyncer stops all syncers for the input service.
func (m *NegBindingSyncerManager) StopServiceSyncers(namespace, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := getServiceKey(namespace, name)
	for bindingRef, state := range m.syncerStateMap {
		if state.serviceKey == key {
			if state.syncer != nil && !state.syncer.IsStopped() {
				state.syncer.Stop()
			}
			delete(m.syncerStateMap, bindingRef)
		}
	}
}

// Sync signals all syncers related to the service to sync.
func (m *NegBindingSyncerManager) Sync(namespace, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := getServiceKey(namespace, name)
	for _, state := range m.syncerStateMap {
		if state.serviceKey == key && state.syncer != nil && !state.syncer.IsStopped() {
			state.syncer.Sync()
		}
	}
}

// SyncAllSyncers signals all syncers to sync.
func (m *NegBindingSyncerManager) SyncAllSyncers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for bindingRef, state := range m.syncerStateMap {
		if state.syncer == nil || state.syncer.IsStopped() {
			m.baseManager.logger.V(1).Info("SyncAllSyncers: Syncer is already stopped; not syncing.", "bindingRef", bindingRef.String())
			continue
		}
		state.syncer.Sync()
	}
}

type NegBindingErrorType string

const (
	InvalidBackendRef = NegBindingErrorType("invalid-backend-ref")
)

type NegBindingError struct {
	Type NegBindingErrorType
}

func (m *NegBindingSyncerManager) getSyncerKey(binding *negbindingv1beta1.NetworkEndpointGroupBinding, service *apiv1.Service) (negtypes.NegSyncerKey, error) {
	portTuple := m.getPortTuple(binding, service)
	if portTuple == nil {
		// TODO(yushkevicha): meaningful err
		return negtypes.NegSyncerKey{}, fmt.Errorf("")
	}

	return negtypes.NegSyncerKey{
		Namespace:        binding.Namespace,
		Name:             binding.Spec.BackendRef.Name,
		NegBindingName:   binding.Name,
		PortTuple:        *portTuple,
		NegType:          negtypes.VmIpPortEndpointType,
		EpCalculatorMode: negtypes.L7Mode,
	}, nil
}

func (m *NegBindingSyncerManager) getPortTuple(binding *negbindingv1beta1.NetworkEndpointGroupBinding, service *apiv1.Service) *negtypes.SvcPortTuple {
	for _, sp := range service.Spec.Ports {
		if sp.Port == binding.Spec.BackendRef.Port {
			return &negtypes.SvcPortTuple{
				Port:       sp.Port,
				Name:       sp.Name,
				TargetPort: sp.TargetPort.String(),
			}
		}
	}
	return nil
}

func (m *NegBindingSyncerManager) statusToSpecNegRef(statusNegRef negbindingv1beta1.StatusNegRef) (negbindingv1beta1.SpecNegRef, error) {
	negLinkId, err := cloud.ParseResourceURL(statusNegRef.ResourceURL)
	if err != nil {
		return negbindingv1beta1.SpecNegRef{}, err
	}
	subnetLinkId, err := cloud.ParseResourceURL(statusNegRef.SubnetURL)
	return negbindingv1beta1.SpecNegRef{
		Name:   negLinkId.Key.Name,
		Subnet: subnetLinkId.Key.Name,
	}, err
}

func (m *NegBindingSyncerManager) bindingToBindingRef(binding *negbindingv1beta1.NetworkEndpointGroupBinding) NegBindingRef {
	return NegBindingRef{
		Name:      binding.Name,
		Namespace: binding.Namespace,
	}
}

// func (m *NegBindingSyncerManager) generateBindingRefToNegMap(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) map[NegBindingRef]sets.Set[string] {
// 	currentlyManagedNegsMap, currentlyManagedSubnetsMap := m.getCurrentlyManagedNegsAndSubnets(bindings)

// 	bindingRefToNegMap := make(map[NegBindingRef]sets.Set[string])
// 	alreadyAddedNegNames := sets.New[string]()

// 	for bindingRef, negNames := range currentlyManagedNegsMap {
// 		alreadyAddedNegNames = alreadyAddedNegNames.Union(negNames)
// 		bindingRefToNegMap[bindingRef] = negNames.Clone()
// 	}

// 	for _, binding := range bindings {
// 		bindingRef := m.bindingToBindingRef(binding)
// 		for _, negRef := range binding.Spec.NetworkEndpointGroups {
// 			if !alreadyAddedNegNames.Has(negRef.Name) && !currentlyManagedSubnetsMap[bindingRef].Has(negRef.Subnet) {
// 				bindingRefToNegMap[bindingRef].Insert(negRef.Name)
// 				alreadyAddedNegNames.Insert(negRef.Name)
// 			}
// 		}
// 	}

// 	return bindingRefToNegMap
// }

// func (m *NegBindingSyncerManager) getCurrentNegToBindingRefMap(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) map[string]NegBindingRef {
// 	// negToBindingMap := make(map[string]NegBindingRef)
// 	// for binding, negNames := range m.bindingToNegMap {
// 	// 	for name, _ := range negNames {
// 	// 		negToBindingMap[name] = binding
// 	// 	}
// 	// }
// 	// return negToBindingMap

// 	negToBindingMap := make(map[string]NegBindingRef)
// 	for _, binding := range bindings {
// 		for _, negRef := range binding.Status.NetworkEndpointGroups {
// 			id, err := cloud.ParseResourceURL(negRef.SelfLink)
// 			if err != nil {
// 				continue
// 			}
// 			negToBindingMap[id.Key.Name] = NegBindingRef{
// 				Name: binding.Name,
// 				Namespace: binding.Namespace,
// 			}
// 		}
// 	}
// 	return negToBindingMap
// }

// func (m *NegBindingSyncerManager) generateRequestedBindingRefToNegMap(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) map[NegBindingRef]sets.Set[string] {
// 	bindingToNegMap := make(map[NegBindingRef]sets.Set[string])
// 	for _, binding := range bindings {
// 		bindingRef := NegBindingRef{
// 			Name: binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		bindingToNegMap[bindingRef] = sets.New[string]()
// 		for _, negRef := range binding.Spec.NetworkEndpointGroups {
// 			bindingToNegMap[bindingRef].Insert(negRef.Name)
// 		}
// 	}
// 	return bindingToNegMap
// }

// func (m *NegBindingSyncerManager) getCurrentlyManagedNegsAndSubnets(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) (map[NegBindingRef]sets.Set[string], map[NegBindingRef]sets.Set[string]) {
// 	currentlyManagedNegs := make(map[NegBindingRef]sets.Set[string])
// 	currentlyManagedSubnets := make(map[NegBindingRef]sets.Set[string])

// 	for _, binding := range bindings {
// 		bindingRef := NegBindingRef{
// 			Name: binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		currentlyManagedNegs[bindingRef] = sets.New[string]()
// 		currentlyManagedSubnets[bindingRef] = sets.New[string]()
// 		for _, statusNegRef := range binding.Status.NetworkEndpointGroups {
// 			negRef, err := m.statusToSpecNegRef(statusNegRef)
// 			if err == nil {
// 				currentlyManagedNegs[bindingRef].Insert(negRef.Name)
// 				currentlyManagedSubnets[bindingRef].Insert(negRef.Subnet)
// 			}
// 		}
// 	}
// 	return currentlyManagedNegs, currentlyManagedSubnets
// }

// func (m *NegBindingSyncerManager) getCurrentlyManagedSubnetsMap(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) map[NegBindingRef]sets.Set[string] {
// 	currentlyManagedSubnetsMap := make(map[NegBindingRef]sets.Set[string])
// 	for _, binding := range bindings {
// 		bindingRef := NegBindingRef{
// 			Name: binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		currentlyManagedSubnetsMap[bindingRef] = sets.New[string]()
// 		for _, negRef := range binding.Status.NetworkEndpointGroups {
// 			id, err := cloud.ParseResourceURL(negRef.SubnetURL)
// 			if err != nil {
// 				continue
// 			}
// 			currentlyManagedSubnetsMap[bindingRef].Insert(id.Key.Name)
// 		}
// 	}
// }

// func (m *NegBindingSyncerManager) managedNegsChanged(bindingRef NegBindingRef, newManagedNegNames sets.Set[string]) bool {
// 	if _, ok := m.bindingToNegMap[bindingRef]; !ok {
// 		return true
// 	}
// 	return !m.bindingToNegMap[bindingRef].Equal(newManagedNegNames)
// }

// func (m *NegBindingSyncerManager) ensureSyncer(bindingRef NegBindingRef, managedNegNames sets.Set[string], portTuple negtypes.SvcPortTuple, networkInfo *network.NetworkInfo) {
// 	syncer, syncerExists := m.syncerMap[bindingRef]
// 	createSyncer := !syncerExists
// 	updateSyncer := syncerExists && m.managedNegsChanged(bindingRef, managedNegNames)

// 	if !createSyncer && !updateSyncer {
// 		return
// 	}

// }

// func (m *NegBindingSyncerManager) getPortTupleForNegBindings(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding, service *apiv1.Service) (map[*negbindingv1beta1.NetworkEndpointGroupBinding]negtypes.SvcPortTuple, map[*negbindingv1beta1.NetworkEndpointGroupBinding]error) {
// 	knownSvcPortSet := make(negtypes.SvcPortTupleSet)
// 	for _, sp := range service.Spec.Ports {
// 		knownSvcPortSet.Insert(
// 			negtypes.SvcPortTuple{
// 				Port:       sp.Port,
// 				Name:       sp.Name,
// 				TargetPort: sp.TargetPort.String(),
// 			},
// 		)
// 	}

// 	portTupleMap := map[*negbindingv1beta1.NetworkEndpointGroupBinding]negtypes.SvcPortTuple{}
// 	backendRefErrors := map[*negbindingv1beta1.NetworkEndpointGroupBinding]error{}
// 	for _, binding := range bindings {
// 		port := binding.Spec.BackendRef.Port
// 		if portInfo, ok := knownSvcPortSet.Get(port); ok {
// 			portTupleMap[binding] = portInfo
// 		} else {
// 			backendRefErrors[binding] = fmt.Errorf("port %v specified in binding doesn't exist in the service", port)
// 		}
// 	}

// 	return portTupleMap, backendRefErrors
// }

// func (m *NegBindingSyncerManager) getKnownSvcPortSet(service *apiv1.Service) negtypes.SvcPortTupleSet {
// 	knownSvcPortSet := make(negtypes.SvcPortTupleSet)
// 	for _, sp := range service.Spec.Ports {
// 		knownSvcPortSet.Insert(
// 			negtypes.SvcPortTuple{
// 				Port:       sp.Port,
// 				Name:       sp.Name,
// 				TargetPort: sp.TargetPort.String(),
// 			},
// 		)
// 	}
// 	return knownSvcPortSet
// }

// func (m *NegBindingSyncerManager) getPortTuple(syncInfo NegSyncInfo, knownSvcPortSet negtypes.SvcPortTupleSet) (negtypes.SvcPortTuple, error) {
// 	portTuple, ok := knownSvcPortSet.Get(syncInfo.Port)
// 	if !ok {
// 		// TODO(yushkevicha): do meaningful
// 		return negtypes.SvcPortTuple{}, fmt.Errorf("")
// 	}
// 	return portTuple, nil
// }

// func (m *NegBindingSyncerManager) getSyncerKey(negBindingName string) (negtypes.NegSyncerKey, error) {
// 	portTuple, err := m.getPortTuple(syncInfo, knownSvcPortSet)
// 	if err != nil {
// 		return negtypes.NegSyncerKey{}, err
// 	}
// 	key := negtypes.NegSyncerKey{
// 		Namespace: syncInfo.Service.namespace,
// 		Name: syncInfo.Service.name,
// 		NegName: negName,
// 		Subnet: syncInfo.Subnet,
// 		PortTuple: portTuple,
// 		NegType: negtypes.VmIpPortEndpointType,
// 		EpCalculatorMode: negtypes.L7Mode,
// 	}
// 	return key, nil
// }

// func (m *NegBindingSyncerManager) getSyncerKeys(binding *NegBindingWithIgnoredNegs, portTuple negtypes.SvcPortTuple) []negtypes.NegSyncerKey {
// 	keys := []negtypes.NegSyncerKey{}
// 	for _, ref := range binding.Spec.NetworkEndpointGroups {
// 		if binding.IgnoredNegs.Has(ref.Name) {
// 			continue
// 		}

// 		key := negtypes.NegSyncerKey{
// 			Namespace:        binding.Namespace,
// 			Name:             binding.Spec.BackendRef.Name,
// 			NegName:          ref.Name,
// 			// subnet
// 			PortTuple:        portTuple,
// 			NegType:          negtypes.VmIpPortEndpointType,
// 			EpCalculatorMode: negtypes.L7Mode,
// 		}
// 		keys = append(keys, key)
// 	}
// 	return keys
// }

// func (m *NegBindingSyncerManager) ensureSyncer0(syncerKey negtypes.NegSyncerKey, networkInfo *network.NetworkInfo) error {
// 	syncer, ok := m.syncerMap[syncerKey]
// 	if !ok {
// 		epc := negsyncer.GetEndpointsCalculator(
// 			m.baseManager.podLister,
// 			m.baseManager.nodeLister,
// 			m.baseManager.serviceLister,
// 			m.baseManager.zoneGetter,
// 			syncerKey,
// 			"",
// 			m.baseManager.logger.WithValues("service", klog.KRef(syncerKey.Namespace, syncerKey.Name), "negName", syncerKey.NegName),
// 			m.baseManager.enableDualStackNEG,
// 			m.baseManager.syncerMetrics,
// 			networkInfo,
// 			"",
// 			m.baseManager.negMetrics,
// 		)

// 		syncer = negsyncer.NewTransactionSyncer(
// 			syncerKey,
// 			m.baseManager.recorder,
// 			m.baseManager.cloud,
// 			m.baseManager.zoneGetter,
// 			m.baseManager.podLister,
// 			m.baseManager.serviceLister,
// 			m.baseManager.endpointSliceLister,
// 			m.baseManager.nodeLister,
// 			m.baseManager.svcNegLister,
// 			m.baseManager.negBindingLister,
// 			m.baseManager.reflector,
// 			epc,
// 			string(m.baseManager.kubeSystemUID),
// 			m.baseManager.svcNegClient,
// 			m.baseManager.negBindingClient,
// 			m.baseManager.syncerMetrics,
// 			true,
// 			m.baseManager.logger,
// 			m.baseManager.lpConfig,
// 			m.baseManager.enableDualStackNEG,
// 			*networkInfo,
// 			NoopNegNamer{},
// 			m.baseManager.negMetrics,
// 		)
// 		m.syncerMap[syncerKey] = syncer
// 	}

// 	if syncer.IsStopped() {
// 		return syncer.Start()
// 	}

// 	return nil
// }

// func (m *NegBindingSyncerManager) deduplicateNegRefs(bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) []*NegBindingWithIgnoredNegs {
// 	alreadyManagedNegs := map[string]NegBindingRef{}

// 	for _, binding := range bindings {
// 		for _, ref := range binding.Status.NetworkEndpointGroups {
// 			id, err := cloud.ParseResourceURL(ref.SelfLink)
// 			if err != nil {
// 				continue
// 			}
// 			alreadyManagedNegs[id.Key.Name] = NegBindingRef{
// 				Name:      binding.Name,
// 				Namespace: binding.Namespace,
// 			}
// 		}
// 	}

// 	slices.SortFunc(bindings, func(negb1, negb2 *negbindingv1beta1.NetworkEndpointGroupBinding) int {
// 		return negb1.CreationTimestamp.Compare(negb2.CreationTimestamp.Time)
// 	})
// 	negNames := sets.New[string]()
// 	res := make([]*NegBindingWithIgnoredNegs, len(bindings))
// 	for i, binding := range bindings {
// 		bindingRef := NegBindingRef{
// 			Name:      binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		res[i] = &NegBindingWithIgnoredNegs{NetworkEndpointGroupBinding: binding, IgnoredNegs: sets.Set[string]{}}
// 		for _, ref := range binding.Spec.NetworkEndpointGroups {
// 			if managedBy := alreadyManagedNegs[ref.Name]; managedBy == bindingRef {
// 				continue
// 			}

// 			if negNames.Has(ref.Name) {
// 				res[i].IgnoredNegs.Insert(ref.Name)
// 			} else {
// 				negNames.Insert(ref.Name)
// 			}
// 		}
// 	}
// 	return res
// }

// func (m *NegBindingSyncerManager) getNegSyncInfoMapUpdate(serviceRef serviceKey, bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) (NegSyncInfoMap, NegSyncInfoMap) {
// 	currentInfoMap := m.getCurrentNegSyncInfoMap(serviceRef, bindings)
// 	newInfoMap := m.getNewNegSyncInfoMap(serviceRef, bindings, currentInfoMap)

// 	addNegSyncInfoMap := make(NegSyncInfoMap)
// 	removeNegSyncInfoMap := make(NegSyncInfoMap)

// 	for negName, currentSyncInfo := range currentInfoMap {
// 		newSyncInfo, ok := newInfoMap[negName]
// 		if !ok {
// 			removeNegSyncInfoMap.Insert(negName, currentSyncInfo)
// 			continue
// 		}
// 		if newSyncInfo != currentSyncInfo {
// 			addNegSyncInfoMap.Insert(negName, newSyncInfo)
// 			removeNegSyncInfoMap.Insert(negName, newSyncInfo)
// 		}
// 	}

// 	for negName, newSyncInfo := range newInfoMap {
// 		_, ok := currentInfoMap[negName]
// 		if !ok {
// 			addNegSyncInfoMap.Insert(negName, newSyncInfo)
// 		}
// 	}

// 	return addNegSyncInfoMap, removeNegSyncInfoMap
// }

// func (m *NegBindingSyncerManager) getCurrentNegSyncInfoMap(serviceRef serviceKey, bindings []*negbindingv1beta1.NetworkEndpointGroupBinding) NegSyncInfoMap {
// 	currentInfoMap := make(NegSyncInfoMap)
// 	for _, binding := range bindings {
// 		for _, ref := range binding.Status.NetworkEndpointGroups {
// 			refId, err := cloud.ParseResourceURL(ref.SelfLink)
// 			if err != nil {
// 				continue
// 			}
// 			subnetId, err := cloud.ParseResourceURL(ref.SubnetURL)
// 			if err != nil {
// 				continue
// 			}

// 			syncInfo := NegSyncInfo{
// 				Service: serviceRef,
// 				Binding: NegBindingRef{
// 					Name: binding.Name,
// 					Namespace: binding.Namespace,
// 				},
// 				Subnet: subnetId.Key.Name,
// 				Port: binding.Spec.BackendRef.Port,
// 			}
// 			currentInfoMap.Insert(refId.Key.Name, syncInfo)
// 		}
// 	}

// 	return currentInfoMap
// }

// func (m *NegBindingSyncerManager) getNewNegSyncInfoMap(serviceRef serviceKey, bindings []*negbindingv1beta1.NetworkEndpointGroupBinding, currentInfoMap NegSyncInfoMap) NegSyncInfoMap {
// 	bindingsByRef := map[NegBindingRef]*negbindingv1beta1.NetworkEndpointGroupBinding{}
// 	for _, binding := range bindings {
// 		bindingref := NegBindingRef{
// 			Name: binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		bindingsByRef[bindingref] = binding
// 	}

// 	newInfoMap := make(NegSyncInfoMap)
// 	slices.SortFunc(bindings, func(negb1, negb2 *negbindingv1beta1.NetworkEndpointGroupBinding) int {
// 		return negb1.CreationTimestamp.Compare(negb2.CreationTimestamp.Time)
// 	})
// 	for _, binding := range bindings {
// 		bindingRef := NegBindingRef{
// 			Name:      binding.Name,
// 			Namespace: binding.Namespace,
// 		}
// 		for _, ref := range binding.Spec.NetworkEndpointGroups {
// 			if newInfoMap.Has(ref.Name) {
// 				continue
// 			}

// 			currentSyncInfo, ok := currentInfoMap.Get(ref.Name)
// 			if ok && m.negSyncInfoStillValid(ref.Name, currentSyncInfo, bindingsByRef) {
// 				newInfoMap.Insert(ref.Name, currentSyncInfo)
// 				continue
// 			}

// 			newSyncInfo := NegSyncInfo{
// 				Service: serviceRef,
// 				Binding: bindingRef,
// 				Subnet: ref.Subnet,
// 				Port: binding.Spec.BackendRef.Port,
// 			}
// 			newInfoMap.Insert(ref.Name, newSyncInfo)
// 		}
// 	}

// 	return newInfoMap
// }

// func (m *NegBindingSyncerManager) negSyncInfoStillValid(negName string, syncInfo NegSyncInfo, bindingsByRef map[NegBindingRef]*negbindingv1beta1.NetworkEndpointGroupBinding) bool {
// 	binding := bindingsByRef[syncInfo.Binding]
// 	for _, negRef := range binding.Spec.NetworkEndpointGroups {
// 		if negRef.Name == negName {
// 			return true
// 		}
// 	}
// 	return false
// }

// func (m *NegBindingSyncerManager) stopObsoleteSyncers(svcRef serviceKey, newNegSyncInfoMap NegSyncInfoMap, knownSvcPortSet negtypes.SvcPortTupleSet) {
// 	for key, syncer := range m.syncerMap[svcRef] {
// 		syncInfo, ok := newNegSyncInfoMap.Get(key.NegName)
// 		if !ok {
// 			delete(m.syncerMap[svcRef], key)
// 			syncer.Stop()
// 			continue
// 		}
// 		newSyncKey, err := m.getSyncerKey(key.Name, syncInfo, knownSvcPortSet)
// 		if err != nil || newSyncKey != key {
// 			delete(m.syncerMap[svcRef], key)
// 			syncer.Stop()
// 		}
// 	}
// }
