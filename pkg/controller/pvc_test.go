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

func TestGetPVCName(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
	}
	if e, a := "volume-foo-0", getPVCName(sts, "volume", 0); e != a {
		t.Errorf("Expected getPVCName() to return %s, but got %s", e, a)
	}
	if e, a := "other-foo-1", getPVCName(sts, "other", 1); e != a {
		t.Errorf("Expected getPVCName() to return %s, but got %s", e, a)
	}

	sts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
	}
	if e, a := "volume-bar-1", getPVCName(sts, "volume", 1); e != a {
		t.Errorf("Expected getPVCName() to return %s, but got %s", e, a)
	}
}

func TestExtractNameAndOrdinal(t *testing.T) {
	cases := []struct {
		pvcName         string
		expectedName    string
		expectedOrdinal int
		expectError     bool
	}{
		{"volume-statefulset-0", "volume-statefulset", 0, false},
		{"volume-statefulset-1", "volume-statefulset", 1, false},
		{"other-sts-1", "other-sts", 1, false},
		{"my-volume-my-sts-0", "my-volume-my-sts", 0, false},
		{"pvc-0", "pvc", 0, false},
		{pvcName: "my-pvc", expectError: true},
		{pvcName: "my-own-pvc", expectError: true},
		{pvcName: "mypvc", expectError: true},
	}

	for _, tc := range cases {
		t.Run(tc.pvcName, func(t *testing.T) {
			name, ordinal, err := extractNameAndOrdinal(tc.pvcName)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error, but extractNameAndOrdinal() did not return it.")
				}
			} else {
				if err != nil {
					t.Errorf("extractNameAndOrdinal() returned an error when it shouldn't have: %s", err)
					return
				}

				if e, a := tc.expectedName, name; e != a {
					t.Errorf("Expected name '%s', but got '%s'", e, a)
				}
				if e, a := tc.expectedOrdinal, ordinal; e != a {
					t.Errorf("Expected ordinal %d, but got %d", e, a)
				}
			}
		})
	}
}

func TestFilterAndGroupClaimsByOrdinal(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mysts"},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "myvolume"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "myothervolume"}},
			},
		},
	}

	cases := []struct {
		name        string
		sts         *appsv1.StatefulSet
		claims      []*corev1.PersistentVolumeClaim
		expectedMap map[int][]*corev1.PersistentVolumeClaim
	}{
		{
			name:        "empty input",
			sts:         sts,
			claims:      []*corev1.PersistentVolumeClaim{},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{},
		},
		{
			name: "smoke",
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newPVC("myvolume-mysts-1"),
				newPVC("myothervolume-mysts-0"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
					newPVC("myothervolume-mysts-0"),
				},
				1: {
					newPVC("myvolume-mysts-1"),
				},
			},
		},
		{
			name: "groups by ordinal",
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newPVC("myvolume-mysts-1"),
				newPVC("myothervolume-mysts-1"),
				newPVC("myothervolume-mysts-2"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
				},
				1: {
					newPVC("myvolume-mysts-1"),
					newPVC("myothervolume-mysts-1"),
				},
				2: {
					newPVC("myothervolume-mysts-2"),
				},
			},
		},
		{
			name: "filters out other statefulsets",
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newPVC("myvolume-othersts-0"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
				},
			},
		},
		{
			name: "filters out volumes not defined on statefulset",
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newPVC("notmyvolume-mysts-0"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
				},
			},
		},
		{
			name: "tolerates PVCs not created by statefulsets", // tolerates == doesn't break when it encounters one
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newPVC("mypvc"),
				newPVC("mypvc1"),
				newPVC("mypvc-1"),
				newPVC("foo-bar"),
				newPVC("foo-bar1"),
				newPVC("foo-bar-1"),
				newPVC("foo-bar-baz"),
				newPVC("foo-bar-baz1"),
				newPVC("foo-bar-baz-1"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
				},
			},
		},
		{
			name: "ignores terminating PVC",
			sts:  sts,
			claims: []*corev1.PersistentVolumeClaim{
				newPVC("myvolume-mysts-0"),
				newTerminatingPVC("myvolume-mysts-1"),
			},
			expectedMap: map[int][]*corev1.PersistentVolumeClaim{
				0: {
					newPVC("myvolume-mysts-0"),
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actualMap := filterAndGroupClaimsByOrdinal(tc.claims, tc.sts)
			if e, a := tc.expectedMap, actualMap; !reflect.DeepEqual(e, a) {
				t.Errorf("Actual map doesn't match expected map\nDiff:\n %s", diff.ObjectReflectDiff(e, a))
			}
		})
	}
}

func newPVC(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func newTerminatingPVC(name string) *corev1.PersistentVolumeClaim {
	pvc := newPVC(name)
	now := metav1.Now()
	pvc.DeletionTimestamp = &now
	return pvc
}
