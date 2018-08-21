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
	appsv1 "k8s.io/api/apps/v1"
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

const annotation = "statefulsets.kubernetes.io/scaledown-pod-template"

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

type fixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset

	// Objects to put in the store.
	pods                   []*corev1.Pod
	statefulSets           []*appsv1.StatefulSet
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

func (f *fixture) addStatefulSets(statefulSets ...*appsv1.StatefulSet) {
	f.statefulSets = append(f.statefulSets, statefulSets...)
	for _, sts := range statefulSets {
		f.kubeObjects = append(f.kubeObjects, sts)
	}
}

func (f *fixture) addPods(pods ...*corev1.Pod) {
	f.pods = append(f.pods, pods...)
	for _, p := range pods {
		f.kubeObjects = append(f.kubeObjects, p)
	}
}

func (f *fixture) addPersistentVolumeClaims(pvcs ...*corev1.PersistentVolumeClaim) {
	f.persistentVolumeClaims = append(f.persistentVolumeClaims, pvcs...)
	for _, pvc := range pvcs {
		f.kubeObjects = append(f.kubeObjects, pvc)
	}
}

func (f *fixture) newController() (*Controller, kubeinformers.SharedInformerFactory) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeObjects...)

	informerFactory := kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	c := NewController(f.kubeclient, informerFactory)

	c.Recorder = &record.FakeRecorder{}

	for _, pod := range f.pods {
		informerFactory.Core().V1().Pods().Informer().GetIndexer().Add(pod)
	}

	for _, sts := range f.statefulSets {
		informerFactory.Apps().V1().StatefulSets().Informer().GetIndexer().Add(sts)
	}

	for _, pvc := range f.persistentVolumeClaims {
		informerFactory.Core().V1().PersistentVolumeClaims().Informer().GetIndexer().Add(pvc)
	}

	return c, informerFactory
}

func (f *fixture) run(sts *appsv1.StatefulSet) {
	f.runController(getKey(sts, f.t), true, false)
}

func (f *fixture) runExpectError(sts *appsv1.StatefulSet) {
	f.runController(getKey(sts, f.t), true, true)
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

	f.expectedActions = f.expectedActions[:0] // clear expectedActions
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
	f.expectCreateAction("pods", p.Namespace, p)
}

func (f *fixture) expectDeletePodAction(namespace, name string) {
	f.expectDeleteAction("pods", namespace, name)
}

func (f *fixture) expectDeletePVCAction(namespace, name string) {
	f.expectDeleteAction("persistentvolumeclaims", namespace, name)
}

func (f *fixture) expectUpdateStatefulSetAction(p *appsv1.StatefulSet) {
	f.expectUpdateAction("statefulsets", p.Namespace, p)
}

func (f *fixture) expectCreateAction(resource string, namespace string, obj runtime.Object) {
	f.expectAction(core.NewCreateAction(schema.GroupVersionResource{Resource: resource}, namespace, obj))
}

func (f *fixture) expectUpdateAction(resource, namespace string, obj runtime.Object) {
	f.expectAction(core.NewUpdateAction(schema.GroupVersionResource{Resource: resource}, namespace, obj))
}

func (f *fixture) expectDeleteAction(resource, namespace, name string) {
	f.expectAction(core.NewDeleteAction(schema.GroupVersionResource{Resource: resource}, namespace, name))
}

func (f *fixture) expectAction(action core.Action) {
	f.expectedActions = append(f.expectedActions, action)
}

func getKey(sts *appsv1.StatefulSet, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(sts)
	if err != nil {
		t.Errorf("Unexpected error getting key for sts %v: %v", sts.Name, err)
		return ""
	}
	return key
}

func TestIgnoresStatefulSetsWithoutVolumeClaimTemplates(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{}
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.run(sts)
}

func TestIgnoresStatefulSetsWithoutCleanupPodTemplate(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	delete(sts.ObjectMeta.Annotations, annotation)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.run(sts)
}

func TestNonJsonPodTemplate(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.ObjectMeta.Annotations[annotation] = "{bad json"
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.runExpectError(sts)
}

func TestInvalidPodTemplateJson(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.ObjectMeta.Annotations[annotation] = `{"spec": {"containers": {"name": "containers should be an array"}}}`
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.runExpectError(sts)
}

// TODO: the controller doesn't return an error in this case, but it should
func Ignored_TestNonPodTemplateJson(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.ObjectMeta.Annotations[annotation] = `{"foo": "this is valid json, but not a pod template"}`
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.runExpectError(sts)
}

func TestDoesNothingWhenPVCsMatchReplicaCount(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(2)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.run(sts)
}

func TestCreatesPodOnScaleDown(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	expectedPod := newCleanupPod(1)
	f.expectCreatePodAction(expectedPod)

	f.run(sts)
}

func TestDoesNotCreatePodOnScaleDownWhenPodAlreadyExists(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	f.addPods(newCleanupPod(1))
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	f.run(sts) // no actions expected
}

func TestCreatesPodOnScaleDownToZero(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(0)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(1)...)

	expectedPod := newCleanupPod(0)
	f.expectCreatePodAction(expectedPod)

	f.run(sts)
}

func TestCreatesOnlyHighestOrdinalPodWhenPodManagementPolicyIsOrderedReady(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(3)...)

	f.expectCreatePodAction(newCleanupPod(2))

	f.run(sts)
}

func TestCreatesMultiplePodsWhenPodManagementPolicyIsParallel(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(3)...)

	f.expectCreatePodAction(newCleanupPod(2))
	f.expectCreatePodAction(newCleanupPod(1))

	f.run(sts)
}

func TestDeletesPodAndClaimOnSuccessfulCompletion(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)

	pod := newCleanupPod(1)
	pod.Status.Phase = corev1.PodSucceeded
	f.addPods(pod)

	f.expectDeletePVCAction(metav1.NamespaceDefault, "data-my-statefulset-1")
	f.expectDeletePodAction(metav1.NamespaceDefault, "my-statefulset-1")
	f.run(sts)
}

func TestMultipleVolumeClaimTemplates(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, newVolumeClaimTemplate("other"))
	sts.Spec.Template.Spec.Containers[0].VolumeMounts = append(sts.Spec.Template.Spec.Containers[0].VolumeMounts, newVolumeMount("other", "/var/other"))
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)
	f.addPersistentVolumeClaims(newCustomPersistentVolumeClaims("other", "my-statefulset", 2)...)

	expectedPod := newCleanupPod(1)
	expectedPod.Spec.Volumes = append(expectedPod.Spec.Volumes, newVolume("other"))
	f.expectCreatePodAction(expectedPod)

	f.run(sts)

	pod := expectedPod.DeepCopy()
	pod.Status.Phase = corev1.PodSucceeded
	f.addPods(pod)

	f.expectDeletePVCAction(metav1.NamespaceDefault, "data-my-statefulset-1")
	f.expectDeletePVCAction(metav1.NamespaceDefault, "other-my-statefulset-1")
	f.expectDeletePodAction(metav1.NamespaceDefault, "my-statefulset-1")

	f.run(sts)
}

// Imagine having a StatefulSet with two volumeClaimTemplates. If a scale-down occurs
// and for some reason one of those PVCs is deleted by someone, should the controller
// create the cleanup pod or not?
// - If it creates it, the pod won't be scheduled because of the missing PVC. If the
//   StatefulSet is then scaled up, the StatefulSet controller will first create the
//   PVC, allowing the cleanup controller to finally run. Once it finishes, the
//   StatefulSet controller will create the regular pod again.
// - If it doesn't create the cleanup pod and then the StatefulSet controller is
//   scaled up, the regular pod will be created instead of the cleanup pod.
// Currently, the controller works as described in the first bullet.
func TestMultipleVolumeClaimTemplatesWithOnePVCMissing(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.Spec.Replicas = int32Ptr(1)
	sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, newVolumeClaimTemplate("other"))
	sts.Spec.Template.Spec.Containers[0].VolumeMounts = append(sts.Spec.Template.Spec.Containers[0].VolumeMounts, newVolumeMount("other", "/var/other"))
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(2)...)
	f.addPersistentVolumeClaims(newCustomPersistentVolumeClaims("other", "my-statefulset", 1)...) // note that PVC with ordinal 1 is missing

	expectedPod := newCleanupPod(1)
	expectedPod.Spec.Volumes = append(expectedPod.Spec.Volumes, newVolume("other"))
	f.expectCreatePodAction(expectedPod)

	f.run(sts)
}

func TestCreatesPodOnStatefulSetDelete(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	now := metav1.Now()
	sts.ObjectMeta.DeletionTimestamp = &now
	f.addStatefulSets(sts)
	f.addPersistentVolumeClaims(newPersistentVolumeClaims(3)...)

	expectedPod := newCleanupPod(2)
	f.expectCreatePodAction(expectedPod)

	f.run(sts)
}

func TestAddsFinalizer(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	sts.ObjectMeta.Finalizers = []string{}
	f.addStatefulSets(sts)
	// NOTE: no PVCs, which means the controller has nothing to do and should remove the finalizer

	updatedSts := sts.DeepCopy()
	updatedSts.ObjectMeta.Finalizers = []string{FinalizerName}
	f.expectUpdateStatefulSetAction(updatedSts)

	f.run(sts)
}

func TestRemovesFinalizerWhenFinished(t *testing.T) {
	f := newFixture(t)
	sts := newStatefulSet()
	now := metav1.Now()
	sts.ObjectMeta.DeletionTimestamp = &now
	f.addStatefulSets(sts)
	// NOTE: no PVCs, which means the controller has nothing to do and should remove the finalizer

	updatedSts := sts.DeepCopy()
	updatedSts.ObjectMeta.Finalizers = []string{}
	f.expectUpdateStatefulSetAction(updatedSts)

	f.run(sts)
}

// TODO: check what happens on scaledown of -2 when pod with ordinal 2 completes (is pod1 created immediately?)
// TODO: StatefulSet deleted while cleanup pod is running
// TODO: cleanup pod has same labels as regular pods (check what happens; do we need to prevent that?)
// TODO: scale down, wait for cleanup pod to run, scale back up and back down - will the sts controller delete the cleanup pod?

func newCleanupPod(ordinal int) *corev1.Pod {
	podTemplate := newCleanupPodTemplateSpec()
	expectedPod := &corev1.Pod{
		ObjectMeta: podTemplate.ObjectMeta,
		Spec:       podTemplate.Spec,
	}
	expectedPod.ObjectMeta.Name = fmt.Sprintf("my-statefulset-%d", ordinal)
	expectedPod.ObjectMeta.Namespace = metav1.NamespaceDefault
	expectedPod.ObjectMeta.Labels["scaledown-pod"] = fmt.Sprintf("my-statefulset-%d", ordinal)
	expectedPod.ObjectMeta.Annotations = map[string]string{
		"statefulsets.kubernetes.io/scaledown-pod-owner": "my-statefulset",
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
	return newCustomPersistentVolumeClaims("data", "my-statefulset", count)
}

func newCustomPersistentVolumeClaims(templateName, statefulSetName string, count int) []*corev1.PersistentVolumeClaim {
	claims := make([]*corev1.PersistentVolumeClaim, count)
	for i := 0; i < count; i++ {
		claims[i] = newPersistentVolumeClaim(templateName, statefulSetName, i)
	}
	return claims
}

func newPersistentVolumeClaim(templateName, statefulSetName string, ordinal int) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%d", templateName, statefulSetName, ordinal),
			Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{
				"app": "my-app",
			},
		},
	}
}

func newStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-statefulset",
			Namespace: metav1.NamespaceDefault,
			Annotations: map[string]string{
				annotation: toJSON(newCleanupPodTemplateSpec()),
			},
			Finalizers: []string{
				FinalizerName,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         "my-service",
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			Replicas:            int32Ptr(2),
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
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/var/data",
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
				},
			},
		},
	}
}

func newCleanupPodTemplateSpec() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "my-cleanup-pod",
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

func newVolumeClaimTemplate(name string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func newVolume(name string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("%s-my-statefulset-%d", name, 1),
			},
		},
	}
}

func newVolumeMount(name, mountPath string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      name,
		MountPath: mountPath,
	}
}

func int32Ptr(i int32) *int32 { return &i }

func toJSON(obj interface{}) string {
	json, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return string(json)
}
