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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/diff"
	"reflect"
	"testing"
)

func TestIsCleanupPod(t *testing.T) {
	cases := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name:     "nil pod",
			pod:      nil,
			expected: false,
		},
		{
			name: "cleanup pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "some-pod",
					Annotations: map[string]string{
						AnnotationStatefulSet: "some-sts",
					},
				},
			},
			expected: true,
		},
		{
			name: "regular pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "some-pod",
					Annotations: map[string]string{
						// no annotation
					},
				},
			},
			expected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if e, a := tc.expected, isCleanupPod(tc.pod); e != a {
				t.Errorf("Expected isCleanupPod() to return %t, but got %t", e, a)
			}
		})
	}
}

func TestGetPodName(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
	}
	if e, a := "foo-0", getPodName(sts, 0); e != a {
		t.Errorf("Expected GetPodName() to return %s, but got %s", e, a)
	}
	if e, a := "foo-1", getPodName(sts, 1); e != a {
		t.Errorf("Expected GetPodName() to return %s, but got %s", e, a)
	}

	sts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
	}
	if e, a := "bar-1", getPodName(sts, 1); e != a {
		t.Errorf("Expected GetPodName() to return %s, but got %s", e, a)
	}
}

func TestNewPod(t *testing.T) {
	cases := []struct {
		name        string
		sts         *appsv1.StatefulSet
		ordinal     int
		expectedPod *corev1.Pod
		expectError bool
	}{
		{
			name: "no template returns error",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "my-sts"},
				// note there's no template annotation
			},
			ordinal:     0,
			expectError: true,
		},
		{
			name: "smoke test",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sts",
					Namespace: "my-namespace",
					Annotations: map[string]string{
						annotation: toJSON(
							&corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"my-label": "my-label-value",
									},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "my-image",
											VolumeMounts: []corev1.VolumeMount{
												{
													Name:      "my-volume",
													MountPath: "/var/data",
												}}}}}},
						)}},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "my-volume"}}}},
			},
			ordinal: 0,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sts-0",
					Namespace: "my-namespace",
					Labels: map[string]string{
						"my-label":      "my-label-value",
						"scaledown-pod": "my-sts-0",
					},
					Annotations: map[string]string{AnnotationStatefulSet: "my-sts"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "my-image",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "my-volume",
									MountPath: "/var/data",
								}}}},
					Volumes: []corev1.Volume{
						{
							Name: "my-volume",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "my-volume-my-sts-0",
								}}}}}},
		},
		{
			name: "no labels in pod template",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts",
					Annotations: map[string]string{
						annotation: toJSON(
							&corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "my-image",
											VolumeMounts: []corev1.VolumeMount{
												{
													Name:      "my-volume",
													MountPath: "/var/data",
												}}}}}},
						)}},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "my-volume"}}}},
			},
			ordinal: 0,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts-0",
					Labels: map[string]string{
						"scaledown-pod": "my-sts-0",
					},
					Annotations: map[string]string{AnnotationStatefulSet: "my-sts"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "my-image",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "my-volume",
									MountPath: "/var/data",
								}}}},
					Volumes: []corev1.Volume{
						{
							Name: "my-volume",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "my-volume-my-sts-0",
								}}}}}},
		},
		{
			name: "multiple volumeClaimTemplates",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts",
					Annotations: map[string]string{
						annotation: toJSON(
							&corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"my-label": "my-label-value",
									},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "my-image",
											VolumeMounts: []corev1.VolumeMount{
												{
													Name:      "my-volume",
													MountPath: "/var/data",
												}}}}}},
						)}},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "my-volume"}},
						{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
					},
				},
			},
			ordinal: 0,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts-0",
					Labels: map[string]string{
						"my-label":      "my-label-value",
						"scaledown-pod": "my-sts-0",
					},
					Annotations: map[string]string{AnnotationStatefulSet: "my-sts"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "my-image",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "my-volume",
									MountPath: "/var/data",
								}}}},
					Volumes: []corev1.Volume{
						{
							Name: "my-volume",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "my-volume-my-sts-0",
								}}},
						{
							Name: "other",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "other-my-sts-0",
								}}}}}},
		},
		{
			name: "restart policy Always is not allowed",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts",
					Annotations: map[string]string{
						annotation: toJSON(
							&corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyAlways,
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "my-image",
											VolumeMounts: []corev1.VolumeMount{
												{
													Name:      "my-volume",
													MountPath: "/var/data",
												}}}}}},
						)}},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "my-volume"}}}},
			},
			ordinal:     0,
			expectError: true,
		},
		{
			name: "restart policy Never is not allowed",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-sts",
					Annotations: map[string]string{
						annotation: toJSON(
							&corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "my-image",
											VolumeMounts: []corev1.VolumeMount{
												{
													Name:      "my-volume",
													MountPath: "/var/data",
												}}}}}},
						)}},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "my-volume"}}}},
			},
			ordinal:     0,
			expectError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := newPod(tc.sts, tc.ordinal)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error, but newPod() did not return it.")
				}
			} else {
				if err != nil {
					t.Errorf("newPod() returned an error when it shouldn't have: %s", err)
					return
				}

				if e, a := tc.expectedPod, actual; !reflect.DeepEqual(e, a) {
					t.Errorf("Actual pod doesn't match expected pod\nDiff:\n %s", diff.ObjectReflectDiff(e, a))
				}
			}
		})
	}
}
