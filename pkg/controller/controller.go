/*
Copyright 2017 The Kubernetes Authors.

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

package controller

import (
	"fmt"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sort"
)

const (
	SuccessCreate    = "SuccessfulCreate"
	DrainSuccess     = "DrainSuccess"
	PVCDeleteSuccess = "SuccessfulPVCDelete"
	PodDeleteSuccess = "SuccessfulDelete"

	MessageDrainPodCreated  = "create Drain Pod %s in StatefulSet %s successful"
	MessageDrainPodFinished = "drain Pod %s in StatefulSet %s completed successfully"
	MessageDrainPodDeleted  = "delete Drain Pod %s in StatefulSet %s successful"
	MessagePVCDeleted       = "delete Claim %s in StatefulSet %s successful"
)

func (c *Controller) processStatefulSet(sts *appsv1.StatefulSet) error {
	// TODO: think about scale-down during a rolling upgrade

	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		// nothing to do, as the stateful pods don't use any PVCs
		glog.Infof("Ignoring StatefulSet '%s' because it does not use any PersistentVolumeClaims.", sts.Name)
		return nil
	}

	if sts.Annotations[AnnotationDrainerPodTemplate] == "" {
		glog.Infof("Ignoring StatefulSet '%s' because it does not define a drain pod template.", sts.Name)
		return nil
	}

	claimsGroupedByOrdinal, err := c.getClaims(sts)
	if err != nil {
		err = fmt.Errorf("Error while getting list of PVCs in namespace %s: %s", sts.Namespace, err)
		glog.Error(err)
		return err
	}

	ordinals := make([]int, 0, len(claimsGroupedByOrdinal))
	for k := range claimsGroupedByOrdinal {
		ordinals = append(ordinals, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ordinals)))

	for _, ordinal := range ordinals {

		// TODO check if the number of claims matches the number of StatefulSet's volumeClaimTemplates. What if it doesn't?

		podName := getPodName(sts, ordinal)
		pod, err := c.podLister.Pods(sts.Namespace).Get(podName)
		if err != nil && !errors.IsNotFound(err) {
			glog.Errorf("Error while getting Pod %s: %s", podName, err)
			return err
		}

		// TODO: scale down to zero? should what happens on such events be configurable? there may or may not be anywhere to drain to
		if int32(ordinal) >= *sts.Spec.Replicas {
			// PVC exists, but its ordinal is higher than the current last stateful pod's ordinal;
			// this means the PVC is an orphan and should be drained & deleted

			// If the Pod doesn't exist, we'll create it
			if pod == nil { // TODO: what if the PVC doesn't exist here (or what if it's deleted just after we create the pod)
				glog.Infof("Found orphaned PVC(s) for ordinal '%d'. Creating drain pod '%s'.", ordinal, podName)

				pod, err := newPod(sts, ordinal)
				if err != nil {
					return fmt.Errorf("Can't create drain Pod object: %s", err)
				}
				pod, err = c.kubeclientset.CoreV1().Pods(sts.Namespace).Create(pod)

				// If an error occurs during Create, we'll requeue the item so we can
				// attempt processing again later. This could have been caused by a
				// temporary network failure, or any other transient reason.
				if err != nil {
					glog.Errorf("Error while creating drain Pod %s: %s", podName, err)
					return err
				}

				c.recorder.Event(sts, corev1.EventTypeNormal, SuccessCreate, fmt.Sprintf(MessageDrainPodCreated, podName, sts.Name))

				if sts.Spec.PodManagementPolicy == appsv1.OrderedReadyPodManagement {
					// don't create additional drain pods; they will be created in one of the
					// next invocations of this method, when the current drain pod finishes
					break
				}

				continue
			}
		}

		// Is it a drain pod or a regular stateful pod?
		if isDrainPod(pod) {
			err = c.deleteDrainPodIfNeeded(sts, pod, ordinal)
			if err != nil {
				return err
			}
		} else {
			// DO nothing. Pod is a regular stateful pod
			//glog.Infof("Pod '%s' exists. Not taking any action.", podName)
			//return nil
		}
	}

	// TODO: add status annotation (what info?)
	return nil
}

func (c *Controller) getClaims(sts *appsv1.StatefulSet) (claimsGroupedByOrdinal map[int][]*corev1.PersistentVolumeClaim, err error) {
	// shouldn't use statefulset.Spec.Selector.MatchLabels, as they don't always match; sts controller looks up pvcs by name!
	allClaims, err := c.pvcLister.PersistentVolumeClaims(sts.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	claims := map[int][]*corev1.PersistentVolumeClaim{}

	for _, pvc := range allClaims {
		if pvc.DeletionTimestamp != nil {
			glog.Infof("PVC '%s' is being deleted. Ignoring it.", pvc.Name)
			continue
		}

		name, ordinal, err := extractNameAndOrdinal(pvc.Name)
		if err != nil {
			continue
		}

		for _, t := range sts.Spec.VolumeClaimTemplates {
			if name == fmt.Sprintf("%s-%s", t.Name, sts.Name) {
				if claims[ordinal] == nil {
					claims[ordinal] = []*corev1.PersistentVolumeClaim{}
				}
				claims[ordinal] = append(claims[ordinal], pvc)
			}
		}
	}

	return claims, nil
}

func (c *Controller) deleteDrainPodIfNeeded(sts *appsv1.StatefulSet, pod *corev1.Pod, ordinal int) error {
	// Drain Pod already exists. Check if it's done draining.
	if pod.Status.Phase == corev1.PodSucceeded {
		glog.Infof("Drain pod '%s' finished.", pod.Name)
		c.recorder.Event(sts, corev1.EventTypeNormal, DrainSuccess, fmt.Sprintf(MessageDrainPodFinished, pod.Name, sts.Name))

		for _, pvcTemplate := range sts.Spec.VolumeClaimTemplates {
			pvcName := getPVCName(sts, pvcTemplate.Name, ordinal)
			glog.Infof("Deleting PVC %s", pvcName)
			err := c.kubeclientset.CoreV1().PersistentVolumeClaims(sts.Namespace).Delete(pvcName, nil)
			if err != nil {
				return err
			}
			c.recorder.Event(sts, corev1.EventTypeNormal, PVCDeleteSuccess, fmt.Sprintf(MessagePVCDeleted, pvcName, sts.Name))
		}

		// TODO what if the user scales up the statefulset and the statefulset controller creates the new pod after we delete the pod but before we delete the PVC
		// TODO what if we crash after we delete the PVC, but before we delete the pod?

		glog.Infof("Deleting drain pod %s", pod.Name)
		err := c.kubeclientset.CoreV1().Pods(sts.Namespace).Delete(pod.Name, nil)
		if err != nil {
			return err
		}
		c.recorder.Event(sts, corev1.EventTypeNormal, PodDeleteSuccess, fmt.Sprintf(MessageDrainPodDeleted, pod.Name, sts.Name))
	}
	return nil
}

func getStatefulSetNameFromPodAnnotation(object metav1.Object) string {
	return object.GetAnnotations()[AnnotationStatefulSet]
}
