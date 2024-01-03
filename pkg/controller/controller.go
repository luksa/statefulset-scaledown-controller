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
	"context"
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
	SuccessCreate  = "SuccessfulCreate"
	CleanupSuccess = "CleanupSuccess"
	DeleteSuccess  = "SuccessfulDelete"

	MessageCleanupPodCreated  = "create Cleanup Pod %s in StatefulSet %s successful"
	MessageCleanupPodFinished = "cleanup Pod %s in StatefulSet %s completed successfully"
	MessageCleanupPodDeleted  = "delete Cleanup Pod %s in StatefulSet %s successful"
	MessagePVCDeleted         = "delete Claim %s in StatefulSet %s successful"

	FinalizerName = "statefulsets.kubernetes.io/scaledown"
)

func (c *Controller) processStatefulSet(sts *appsv1.StatefulSet) error {
	// TODO: think about scale-down during a rolling upgrade

	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		// nothing to do, as the stateful pods don't use any PVCs
		glog.Infof("Ignoring StatefulSet '%s' because it does not use any PersistentVolumeClaims.", sts.Name)
		return nil
	}

	if sts.Annotations[AnnotationScaledownPodTemplate] == "" {
		glog.Infof("Ignoring StatefulSet '%s' because it does not define a scaledown pod template.", sts.Name)
		return nil
	}

	claimsGroupedByOrdinal, err := c.getClaims(sts)
	if err != nil {
		return fmt.Errorf("Error getting list of PVCs in namespace %s: %s", sts.Namespace, err)
	}

	ordinals := extractOrdinals(claimsGroupedByOrdinal)
	sort.Sort(sort.Reverse(sort.IntSlice(ordinals)))

	finalizers := sts.ObjectMeta.Finalizers
	statefulSetTerminating := sts.ObjectMeta.DeletionTimestamp != nil

	if !statefulSetTerminating && !hasFinalizer(sts) {
		// TODO: the finalizer should be added in a mutating webhook or initializer, not here (though adding it here is magnitudes easier)
		glog.Infof("Adding finalizer to StatefulSet %s/%s", sts.Namespace, sts.Name)

		sts.ObjectMeta.Finalizers = append(sts.ObjectMeta.Finalizers, FinalizerName)
		_, err := c.kubeclient.AppsV1().StatefulSets(sts.Namespace).Update(context.TODO(), sts, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("Error adding finalizer to StatefulSet %s/%s: %s", sts.Namespace, sts.Name, err)
		}
		return nil
	}

	if len(ordinals) == 0 && statefulSetTerminating && len(finalizers) > 0 && finalizers[0] == FinalizerName {
		err := c.removeFinalizer(sts)
		if err != nil {
			return fmt.Errorf("Error removing finalizer from StatefulSet %s/%s: %s", sts.Namespace, sts.Name, err)
		}
		return nil
	}

	for _, ordinal := range ordinals {
		podName := getPodName(sts, ordinal)
		pod, err := c.podLister.Pods(sts.Namespace).Get(podName)
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("Error getting Pod %s: %s", podName, err)
		}

		if int32(ordinal) >= *sts.Spec.Replicas || statefulSetTerminating {
			// PVC exists, but its ordinal is higher than the current last stateful pod's ordinal;
			// this means the PVC is an orphan and should be cleaned up & deleted

			// If the Pod doesn't exist, we'll create it
			if pod == nil { // TODO: what if the PVC doesn't exist here (or what if it's deleted just after we create the pod)
				glog.Infof("Found orphaned PVC(s) for ordinal '%d'. Creating cleanup pod '%s'.", ordinal, podName)

				err = c.createCleanupPod(sts, ordinal)
				if err != nil {
					return err
				}

				if sts.Spec.PodManagementPolicy == appsv1.OrderedReadyPodManagement {
					// don't create additional cleanup pods; they will be created in one of the
					// next invocations of this method, when the current cleanup pod finishes
					break
				}

				continue
			}
		}

		if isCleanupPod(pod) && pod.Status.Phase == corev1.PodSucceeded {
			glog.Infof("Cleanup pod '%s' finished. Deleting it and its PVCs.", pod.Name)
			err = c.deleteCleanupPodAndClaims(sts, pod, ordinal)
			if err != nil {
				return err
			}
		}
	}

	// TODO: add status annotation (what info?)
	return nil
}

func hasFinalizer(sts *appsv1.StatefulSet) bool {
	for _, f := range sts.ObjectMeta.Finalizers {
		if f == FinalizerName {
			return true
		}
	}
	return false
}

func (c *Controller) getClaims(sts *appsv1.StatefulSet) (claimsGroupedByOrdinal map[int][]*corev1.PersistentVolumeClaim, err error) {
	// shouldn't use statefulset.Spec.Selector.MatchLabels, as they don't always match; sts controller looks up pvcs by name!
	allClaims, err := c.pvcLister.PersistentVolumeClaims(sts.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	return filterAndGroupClaimsByOrdinal(allClaims, sts), nil
}

func (c *Controller) createCleanupPod(sts *appsv1.StatefulSet, ordinal int) error {
	pod, err := newPod(sts, ordinal)
	if err != nil {
		return fmt.Errorf("Can't create cleanup Pod object: %s", err)
	}
	pod, err = c.kubeclient.CoreV1().Pods(sts.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})

	// If an error occurs during Create, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return fmt.Errorf("Error creating cleanup Pod: %s", err)
	}

	c.Recorder.Event(sts, corev1.EventTypeNormal, SuccessCreate, fmt.Sprintf(MessageCleanupPodCreated, pod.Name, sts.Name))
	return nil
}

func (c *Controller) deleteCleanupPodAndClaims(sts *appsv1.StatefulSet, pod *corev1.Pod, ordinal int) error {
	c.Recorder.Event(sts, corev1.EventTypeNormal, CleanupSuccess, fmt.Sprintf(MessageCleanupPodFinished, pod.Name, sts.Name))

	for _, pvcTemplate := range sts.Spec.VolumeClaimTemplates {
		pvcName := getPVCName(sts, pvcTemplate.Name, ordinal)
		glog.Infof("Deleting PVC %s", pvcName)
		err := c.kubeclient.CoreV1().PersistentVolumeClaims(sts.Namespace).Delete(context.TODO(), pvcName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		c.Recorder.Event(sts, corev1.EventTypeNormal, DeleteSuccess, fmt.Sprintf(MessagePVCDeleted, pvcName, sts.Name))
	}

	// TODO what if the user scales up the statefulset and the statefulset controller creates the new pod after we delete the pod but before we delete the PVC
	// TODO what if we crash after we delete the PVC, but before we delete the pod?

	glog.Infof("Deleting cleanup pod %s", pod.Name)
	err := c.kubeclient.CoreV1().Pods(sts.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	c.Recorder.Event(sts, corev1.EventTypeNormal, DeleteSuccess, fmt.Sprintf(MessageCleanupPodDeleted, pod.Name, sts.Name))
	return nil
}

func getStatefulSetNameFromPodAnnotation(object metav1.Object) string {
	return object.GetAnnotations()[AnnotationStatefulSet]
}

func extractOrdinals(m map[int][]*corev1.PersistentVolumeClaim) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TODO: prevent sts controller from deleting pod-1 while pod-2 cleanup pod is running (using finalizers on pods?)

func (c *Controller) removeFinalizer(sts *appsv1.StatefulSet) error {
	glog.Infof("Removing finalizer from StatefulSet %s/%s", sts.Namespace, sts.Name)
	if sts.ObjectMeta.Finalizers[0] != FinalizerName {
		panic("should never come to this")
	}

	sts.ObjectMeta.Finalizers = sts.ObjectMeta.Finalizers[1:]
	_, err := c.kubeclient.AppsV1().StatefulSets(sts.Namespace).Update(context.TODO(), sts, metav1.UpdateOptions{})
	return err
}
