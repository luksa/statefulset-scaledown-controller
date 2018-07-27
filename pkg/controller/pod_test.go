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
	"testing"
)

func TestIsDrainPod(t *testing.T) {
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
			name: "drain pod",
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
			if e, a := tc.expected, isDrainPod(tc.pod); e != a {
				t.Errorf("Expected isDrainPod() to return %t, but got %t", e, a)
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
