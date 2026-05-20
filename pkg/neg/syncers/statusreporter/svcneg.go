/*
Copyright 2024 The Kubernetes Authors.

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

package statusreporter

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	negv1beta1 "k8s.io/ingress-gce/pkg/apis/svcneg/v1beta1"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/flags"
	"k8s.io/ingress-gce/pkg/neg/metrics"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	svcnegclient "k8s.io/ingress-gce/pkg/svcneg/client/clientset/versioned"
	"k8s.io/ingress-gce/pkg/utils/patch"
	"k8s.io/klog/v2"
)

const (
	NegSyncSuccessful           = "NegSyncSuccessful"
	NegSyncFailed               = "NegSyncFailed"
	NegInitializationSuccessful = "NegInitializationSuccessful"
	NegInitializationFailed     = "NegInitializationFailed"
)

type SvcNegStatusReporter struct {
	Namespace    string
	SvcNegName   string
	SvcNegClient svcnegclient.Interface
	SvcNegLister cache.Indexer
	ZoneGetter   negtypes.ZoneGetter
	SubnetURL    string
	NegMetrics   *metrics.NegMetrics
	Logger       klog.Logger
}

func NewSvcNegStatusReporter(
	namespace string,
	svcNegName string,
	svcNegClient svcnegclient.Interface,
	svcNegLister cache.Indexer,
	zoneGetter negtypes.ZoneGetter,
	subnetURL string,
	negMetrics *metrics.NegMetrics,
	logger klog.Logger,
) negtypes.StatusReporter {
	return &SvcNegStatusReporter{
		Namespace:    namespace,
		SvcNegName:   svcNegName,
		SvcNegClient: svcNegClient,
		SvcNegLister: svcNegLister,
		ZoneGetter:   zoneGetter,
		SubnetURL:    subnetURL,
		NegMetrics:   negMetrics,
		Logger:       logger.WithName("SvcNegStatusReporter"),
	}
}

// Implement negtypes.StatusReporter interface

func (r *SvcNegStatusReporter) SyncFinished(syncErr error) (bool, error) {
	if r.SvcNegClient == nil {
		return false, nil
	}
	origSvcNeg, err := r.getFromStore()
	if err != nil {
		r.Logger.Error(err, "Error updating status for svcneg, failed to get svcneg from store")
		r.NegMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return false, err
	}
	svcNeg := origSvcNeg.DeepCopy()

	ts := metav1.Now()
	needInit := false
	if _, _, exists := r.findCondition(svcNeg.Status.Conditions, negv1beta1.Initialized); !exists {
		needInit = true
	}
	r.NegMetrics.PublishNegSyncerStalenessMetrics(ts.Sub(svcNeg.Status.LastSyncTime.Time))

	r.ensureCondition(svcNeg, r.getSyncedCondition(syncErr))
	svcNeg.Status.LastSyncTime = ts

	if len(svcNeg.Status.NetworkEndpointGroups) == 0 {
		needInit = true
	}

	_, err = r.patchStatus(origSvcNeg.Status, svcNeg.Status)
	if err != nil {
		r.Logger.Error(err, "Error updating SvcNeg CR")
		r.NegMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return needInit, err
	}
	return needInit, nil
}

func (r *SvcNegStatusReporter) NEGsEnsured(negs []*composite.NetworkEndpointGroup, errList []error) error {
	if r.SvcNegClient == nil {
		return nil
	}

	origSvcNeg, err := r.getFromStore()
	if err != nil {
		r.Logger.Error(err, "Error updating init status for svcneg, failed to get svcneg from store.")
		r.NegMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return err
	}

	svcNeg := origSvcNeg.DeepCopy()
	var negObjRefs []negv1beta1.NegObjectReference
	for _, negObj := range negs {
		negRef := negv1beta1.NegObjectReference{
			Id:                  fmt.Sprint(negObj.Id),
			SelfLink:            negObj.SelfLink,
			NetworkEndpointType: negv1beta1.NetworkEndpointType(negObj.NetworkEndpointType),
		}
		if flags.F.EnableMultiSubnetClusterPhase1 {
			negRef.State = negv1beta1.ActiveState
			negRef.SubnetURL = negObj.Subnetwork
		}
		negObjRefs = append(negObjRefs, negRef)
	}

	if flags.F.EnableMultiSubnetClusterPhase1 {
		nonActiveNegRefs := r.getNonActiveNegRefs(origSvcNeg.Status.NetworkEndpointGroups, negObjRefs)
		negObjRefs = append(negObjRefs, nonActiveNegRefs...)
	}
	svcNeg.Status.NetworkEndpointGroups = negObjRefs

	initializedCondition := r.getInitializedCondition(utilerrors.NewAggregate(errList))
	finalCondition := r.ensureCondition(svcNeg, initializedCondition)
	r.NegMetrics.PublishNegInitializationMetrics(finalCondition.LastTransitionTime.Sub(origSvcNeg.GetCreationTimestamp().Time))

	_, err = r.patchStatus(origSvcNeg.Status, svcNeg.Status)
	if err != nil {
		r.Logger.Error(err, "Error updating SvcNeg CR")
		r.NegMetrics.PublishNegControllerErrorCountMetrics(err, true)
		return err
	}
	return nil
}

func (r *SvcNegStatusReporter) NetworkEndpointGroups() ([]*composite.NetworkEndpointGroup, error) {
	svcNegCR, err := r.getFromStore()
	if err != nil {
		return nil, err
	}
	var negs []*composite.NetworkEndpointGroup
	for _, ref := range svcNegCR.Status.NetworkEndpointGroups {
		id, err := strconv.ParseUint(ref.Id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse NEG ID %q: %w", ref.Id, err)
		}
		negs = append(negs, &composite.NetworkEndpointGroup{
			Id:                  id,
			SelfLink:            ref.SelfLink,
			NetworkEndpointType: string(ref.NetworkEndpointType),
			Subnetwork:          ref.SubnetURL,
		})
	}
	return negs, nil
}

func (r *SvcNegStatusReporter) LastSyncTime() (time.Time, error) {
	svcNegCR, err := r.getFromStore()
	if err != nil {
		return time.Time{}, err
	}
	return svcNegCR.Status.LastSyncTime.Time, nil
}

// Helper methods

func (r *SvcNegStatusReporter) getFromStore() (*negv1beta1.ServiceNetworkEndpointGroup, error) {
	n, exists, err := r.SvcNegLister.GetByKey(fmt.Sprintf("%s/%s", r.Namespace, r.SvcNegName))
	if err != nil {
		return nil, fmt.Errorf("error getting svcneg %s/%s from cache: %w", r.Namespace, r.SvcNegName, err)
	}
	if !exists {
		return nil, fmt.Errorf("svcneg %s/%s is not in store", r.Namespace, r.SvcNegName)
	}

	return n.(*negv1beta1.ServiceNetworkEndpointGroup), nil
}

func (r *SvcNegStatusReporter) patchStatus(oldStatus, newStatus negv1beta1.ServiceNetworkEndpointGroupStatus) (*negv1beta1.ServiceNetworkEndpointGroup, error) {
	patchBytes, err := patch.MergePatchBytes(negv1beta1.ServiceNetworkEndpointGroup{Status: oldStatus}, negv1beta1.ServiceNetworkEndpointGroup{Status: newStatus})
	if err != nil {
		return nil, fmt.Errorf("failed to prepare patch bytes: %w", err)
	}

	start := time.Now()
	svcNeg, err := r.SvcNegClient.NetworkingV1beta1().ServiceNetworkEndpointGroups(r.Namespace).Patch(context.Background(), r.SvcNegName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	r.NegMetrics.PublishK8sRequestCountMetrics(start, metrics.PatchRequest, err)
	return svcNeg, err
}

func (r *SvcNegStatusReporter) ensureCondition(svcNeg *negv1beta1.ServiceNetworkEndpointGroup, expectedCondition negv1beta1.Condition) negv1beta1.Condition {
	condition, index, exists := r.findCondition(svcNeg.Status.Conditions, expectedCondition.Type)
	if !exists {
		svcNeg.Status.Conditions = append(svcNeg.Status.Conditions, expectedCondition)
		return expectedCondition
	}

	if condition.Status == expectedCondition.Status {
		expectedCondition.LastTransitionTime = condition.LastTransitionTime
	}

	svcNeg.Status.Conditions[index] = expectedCondition
	return expectedCondition
}

func (r *SvcNegStatusReporter) findCondition(conditions []negv1beta1.Condition, conditionType string) (negv1beta1.Condition, int, bool) {
	for i, c := range conditions {
		if c.Type == conditionType {
			return c, i, true
		}
	}

	return negv1beta1.Condition{}, -1, false
}

func (r *SvcNegStatusReporter) getSyncedCondition(err error) negv1beta1.Condition {
	if err != nil {
		return negv1beta1.Condition{
			Type:               negv1beta1.Synced,
			Status:             v1.ConditionFalse,
			Reason:             NegSyncFailed,
			LastTransitionTime: metav1.Now(),
			Message:            err.Error(),
		}
	}

	return negv1beta1.Condition{
		Type:               negv1beta1.Synced,
		Status:             v1.ConditionTrue,
		Reason:             NegSyncSuccessful,
		LastTransitionTime: metav1.Now(),
	}
}

func (r *SvcNegStatusReporter) getInitializedCondition(err error) negv1beta1.Condition {
	if err != nil {
		return negv1beta1.Condition{
			Type:               negv1beta1.Initialized,
			Status:             v1.ConditionFalse,
			Reason:             NegInitializationFailed,
			LastTransitionTime: metav1.Now(),
			Message:            err.Error(),
		}
	}

	return negv1beta1.Condition{
		Type:               negv1beta1.Initialized,
		Status:             v1.ConditionTrue,
		Reason:             NegInitializationSuccessful,
		LastTransitionTime: metav1.Now(),
	}
}

func (r *SvcNegStatusReporter) getNonActiveNegRefs(oldNegRefs []negv1beta1.NegObjectReference, currentNegRefs []negv1beta1.NegObjectReference) []negv1beta1.NegObjectReference {
	subnetConfigs := r.ZoneGetter.ListSubnets(r.Logger)
	subnetMap := make(map[string]struct{})
	for _, subnet := range subnetConfigs {
		subnetMap[subnet.Name] = struct{}{}
	}

	activeNegs := make(map[negtypes.NegInfo]struct{})
	for _, negRef := range currentNegRefs {
		negInfo, err := negtypes.NegInfoFromNegRef(negRef)
		if err != nil {
			r.Logger.Error(err, "Failed to extract name and zone information of a neg from the current snapshot", "negId", negRef.Id, "negSelfLink", negRef.SelfLink)
			continue
		}
		activeNegs[negInfo] = struct{}{}
	}

	var nonActiveNegRefs []negv1beta1.NegObjectReference
	for _, origNegRef := range oldNegRefs {
		negInfo, err := negtypes.NegInfoFromNegRef(origNegRef)
		if err != nil {
			r.Logger.Error(err, "Failed to extract name and zone information of a neg from the previous snapshot, skipping validating if it is an Inactive NEG", "negId", origNegRef.Id, "negSelfLink", origNegRef.SelfLink)
			continue
		}

		if _, exists := activeNegs[negInfo]; exists {
			continue
		}

		nonActiveNegRef := origNegRef.DeepCopy()
		nonActiveNegRef.State = negv1beta1.InactiveState

		if nonActiveNegRef.SubnetURL == "" {
			nonActiveNegRef.SubnetURL = r.SubnetURL
		}

		resID, err := cloud.ParseResourceURL(nonActiveNegRef.SubnetURL)
		if err != nil {
			r.Logger.Error(err, "Failed to extract subnet information from the previous snapshot, skipping validating if it is an Inactive or to-be-deleted NEG", "negId", nonActiveNegRef.Id, "negSelfLink", nonActiveNegRef.SelfLink)
			continue
		}

		if _, exists := subnetMap[resID.Key.Name]; !exists {
			nonActiveNegRef.State = negv1beta1.ToBeDeletedState
		}

		nonActiveNegRefs = append(nonActiveNegRefs, *nonActiveNegRef)
	}
	return nonActiveNegRefs
}
