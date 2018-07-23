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
	"reflect"
	"testing"
	"time"

	"encoding/json"
	"fmt"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/diff"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

const annotation = "statefulsets.kubernetes.io/drainer-pod-template"

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

type fixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset

	// Objects to put in the store.
	statefulSets           []*apps.StatefulSet
	persistentVolumeClaims []*corev1.PersistentVolumeClaim

	// Actions expected to happen on the client.
	expectedActions []core.Action

	// Objects from here preloaded into NewSimpleFake.
	kubeObjects []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.kubeObjects = []runtime.Object{}
	return f
}

func (f *fixture) newController() (*Controller, kubeinformers.SharedInformerFactory) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeObjects...)

	informerFactory := kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	c := NewController(f.kubeclient, informerFactory)

	c.recorder = &record.FakeRecorder{}

	for _, sts := range f.statefulSets {
		informerFactory.Apps().V1().StatefulSets().Informer().GetIndexer().Add(sts)
	}

	for _, pvc := range f.persistentVolumeClaims {
		informerFactory.Core().V1().PersistentVolumeClaims().Informer().GetIndexer().Add(pvc)
	}

	return c, informerFactory
}

func (f *fixture) run(key string) {
	f.runController(key, true, false)
}

func (f *fixture) runExpectError(key string) {
	f.runController(key, true, true)
}

func (f *fixture) runController(key string, startInformers bool, expectError bool) {
	c, informerFactory := f.newController()
	if startInformers {
		stopCh := make(chan struct{})
		defer close(stopCh)
		informerFactory.Start(stopCh)
	}

	err := c.syncHandler(key)
	if !expectError && err != nil {
		f.t.Errorf("error syncing StatefulSet: %v", err)
	} else if expectError && err == nil {
		f.t.Error("expected error syncing StatefulSet, got nil")
	}

	k8sActions := filterInformerActions(f.kubeclient.Actions())
	for i, action := range k8sActions {
		if len(f.expectedActions) < i+1 {
			f.t.Errorf("%d unexpected actions: %+v", len(k8sActions)-len(f.expectedActions), k8sActions[i:])
			break
		}

		expectedAction := f.expectedActions[i]
		checkAction(expectedAction, action, f.t)
	}

	if len(f.expectedActions) > len(k8sActions) {
		f.t.Errorf("%d additional expected actions:%+v", len(f.expectedActions)-len(k8sActions), f.expectedActions[len(k8sActions):])
	}
}

// checkAction verifies that expected and actual actions are equal and both have
// same attached resources
func checkAction(expected, actual core.Action, t *testing.T) {
	if !(expected.Matches(actual.GetVerb(), actual.GetResource().Resource) && actual.GetSubresource() == expected.GetSubresource()) {
		t.Errorf("Expected\n\t%#v\ngot\n\t%#v", expected, actual)
		return
	}

	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		t.Errorf("Action has wrong type. Expected: %t. Got: %t", expected, actual)
		return
	}

	switch a := actual.(type) {
	case core.CreateAction:
		e, _ := expected.(core.CreateAction)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectReflectDiff(expObject, object))
		}
	case core.UpdateAction:
		e, _ := expected.(core.UpdateAction)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectReflectDiff(expObject, object))
		}
	case core.PatchAction:
		e, _ := expected.(core.PatchAction)
		expPatch := e.GetPatch()
		patch := a.GetPatch()

		if !reflect.DeepEqual(expPatch, expPatch) {
			t.Errorf("Action %s %s has wrong patch\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectReflectDiff(expPatch, patch))
		}
	}
}

// filterInformerActions filters list and watch actions for testing resources.
// Since list and watch don't change resource state we can filter it to lower
// nose level in our tests.
func filterInformerActions(actions []core.Action) []core.Action {
	ret := []core.Action{}
	for _, action := range actions {
		if len(action.GetNamespace()) == 0 &&
			(action.Matches("list", "statefulsets") ||
				action.Matches("watch", "statefulsets") ||
				action.Matches("list", "pods") ||
				action.Matches("watch", "pods") ||
				action.Matches("list", "persistentvolumeclaims") ||
				action.Matches("watch", "persistentvolumeclaims")) {
			continue
		}
		ret = append(ret, action)
	}

	return ret
}

func (f *fixture) expectCreatePodAction(p *corev1.Pod) {
	f.expectedActions = append(f.expectedActions, core.NewCreateAction(schema.GroupVersionResource{Resource: "pods"}, p.Namespace, p))
}

func (f *fixture) expectUpdateStatefulSetStatusAction(sts *apps.StatefulSet) {
	action := core.NewUpdateAction(schema.GroupVersionResource{Resource: "statefulsets"}, sts.Namespace, sts)
	f.expectedActions = append(f.expectedActions, action)
}

func getKey(sts *apps.StatefulSet, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(sts)
	if err != nil {
		t.Errorf("Unexpected error getting key for sts %v: %v", sts.Name, err)
		return ""
	}
	return key
}

func TestCreatesPodOnScaleDown(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	f.statefulSets = append(f.statefulSets, sts)
	f.persistentVolumeClaims = append(f.persistentVolumeClaims, newPersistentVolumeClaims(2)...)

	expectedPod := newDrainPod(1)
	f.expectCreatePodAction(expectedPod)

	f.run(getKey(sts, t))
}

func TestCreatesPodOnScaleDownToZero(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(0)
	f.statefulSets = append(f.statefulSets, sts)
	f.persistentVolumeClaims = append(f.persistentVolumeClaims, newPersistentVolumeClaims(1)...)

	expectedPod := newDrainPod(0)
	f.expectCreatePodAction(expectedPod)

	f.run(getKey(sts, t))
}

func newDrainPod(ordinal int) *corev1.Pod {
	podTemplate := newDrainPodTemplateSpec()
	expectedPod := &corev1.Pod{
		ObjectMeta: podTemplate.ObjectMeta,
		Spec:       podTemplate.Spec,
	}
	expectedPod.ObjectMeta.Name = fmt.Sprintf("my-statefulset-%d", ordinal)
	expectedPod.ObjectMeta.Namespace = metav1.NamespaceDefault
	expectedPod.ObjectMeta.Labels["drain-pod"] = fmt.Sprintf("my-statefulset-%d", ordinal)
	expectedPod.ObjectMeta.Annotations = map[string]string{
		"statefulsets.kubernetes.io/drainer-pod-owner": "my-statefulset",
	}
	expectedPod.Spec.Volumes = append(expectedPod.Spec.Volumes, corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("data-my-statefulset-%d", ordinal),
			},
		},
	})
	expectedPod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	return expectedPod
}

func newPersistentVolumeClaims(count int) []*corev1.PersistentVolumeClaim {
	claims := make([]*corev1.PersistentVolumeClaim, count)
	for i := 0; i < count; i++ {
		claims[i] = newPersistentVolumeClaim(fmt.Sprintf("data-my-statefulset-%d", i))
	}
	return claims
}

func newPersistentVolumeClaim(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{
				"app": "my-app",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
}

func newStatefulSet() *apps.StatefulSet {
	drainPodTemplateJson, err := json.Marshal(newDrainPodTemplateSpec())
	if err != nil {
		panic(err)
	}

	return &apps.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-statefulset",
			Namespace: metav1.NamespaceDefault,
			Annotations: map[string]string{
				annotation: string(drainPodTemplateJson),
			},
		},
		Spec: apps.StatefulSetSpec{
			ServiceName: "my-service",
			Replicas:    int32Ptr(2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "my-app",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "busybox",
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				},
			},
		},
	}
}

func newDrainPodTemplateSpec() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "my-drainer",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "busybox",
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/var/data",
						},
					},
				},
			},
		},
	}
}

func int32Ptr(i int32) *int32 { return &i }
