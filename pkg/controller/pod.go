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

	"encoding/json"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const AnnotationStatefulSet = "statefulsets.kubernetes.io/scaledown-pod-owner" // TODO: can we replace this with an OwnerReference with the StatefulSet as the owner?
const AnnotationSclaedownPodTemplate = "statefulsets.kubernetes.io/scaledown-pod-template"

const LabelScaledownPod = "scaledown-pod"

func newPod(sts *appsv1.StatefulSet, ordinal int) (*corev1.Pod, error) {

	podTemplateJson := sts.Annotations[AnnotationSclaedownPodTemplate]
	if podTemplateJson == "" {
		return nil, fmt.Errorf("No cleanup pod template configured for StatefulSet %s.", sts.Name)
	}
	pod := corev1.Pod{}
	err := json.Unmarshal([]byte(podTemplateJson), &pod)
	if err != nil {
		return nil, fmt.Errorf("Can't unmarshal ScaledownPodTemplate JSON from annotation: %s", err)
	}

	pod.Name = getPodName(sts, ordinal)
	pod.Namespace = sts.Namespace

	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelScaledownPod] = pod.Name

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationStatefulSet] = sts.Name

	// TODO: cannot set blockOwnerDeletion if an ownerReference refers to a resource you can't set finalizers on: User "system:serviceaccount:kube-system:statefulset-scaledown-controller" cannot update statefulsets/finalizers.apps
	//if pod.OwnerReferences == nil {
	//	pod.OwnerReferences = []metav1.OwnerReference{}
	//}
	//pod.OwnerReferences = append(pod.OwnerReferences, *metav1.NewControllerRef(sts, schema.GroupVersionKind{
	//	Group:   appsv1beta1.SchemeGroupVersion.Group,
	//	Version: appsv1beta1.SchemeGroupVersion.Version,
	//	Kind:    "StatefulSet",
	//}))

	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	} else if pod.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		return nil, fmt.Errorf("Scaledown pod template must use restartPolicy: OnFailure")
	}

	for _, pvcTemplate := range sts.Spec.VolumeClaimTemplates {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{ // TODO: override existing volumes with the same name
			Name: pvcTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: getPVCName(sts, pvcTemplate.Name, ordinal),
				},
			},
		})
	}

	return &pod, nil
}

func isCleanupPod(pod *corev1.Pod) bool {
	return pod != nil && pod.ObjectMeta.Annotations[AnnotationStatefulSet] != ""
}

func getPodName(sts *appsv1.StatefulSet, ordinal int) string {
	return fmt.Sprintf("%s-%d", sts.Name, ordinal)
}
