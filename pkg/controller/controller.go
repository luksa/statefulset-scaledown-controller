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
	"time"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"encoding/json"
	"k8s.io/apimachinery/pkg/labels"
	"sort"
	"strings"
	"strconv"
)

const controllerAgentName = "statefulset-drain-controller"
const AnnotationStatefulSet = "statefulsets.kubernetes.io/drainer-pod-owner" // TODO: can we replace this with an OwnerReference with the StatefulSet as the owner?
const AnnotationDrainerPodTemplate = "statefulsets.kubernetes.io/drainer-pod-template"

const LabelDrainPod = "drain-pod"

const (
	SuccessCreate = "SuccessfulCreate"
	DrainSuccess = "DrainSuccess"
	PVCDeleteSuccess = "SuccessfulPVCDelete"
	PodDeleteSuccess = "SuccessfulDelete"

	MessageDrainPodCreated  = "create Drain Pod %s in StatefulSet %s successful"
	MessageDrainPodFinished = "drain Pod %s in StatefulSet %s completed successfully"
	MessageDrainPodDeleted  = "delete Drain Pod %s in StatefulSet %s successful"
	MessagePVCDeleted       = "delete Claim %s in StatefulSet %s successful"
)

type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	statefulSetLister  appslisters.StatefulSetLister
	statefulSetsSynced cache.InformerSynced
	pvcLister          corelisters.PersistentVolumeClaimLister
	pvcsSynched        cache.InformerSynced
	podLister          corelisters.PodLister
	podsSynced         cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	kubeclientset kubernetes.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory) *Controller {

	// obtain references to shared index informers for the Deployment and Foo
	// types.
	statefulSetInformer := kubeInformerFactory.Apps().V1().StatefulSets()
	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	podInformer := kubeInformerFactory.Core().V1().Pods()

	// Create event broadcaster
	// Add statefulset-drain-controller types to the default Kubernetes Scheme so Events can be
	// logged for statefulset-drain-controller types.
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:      kubeclientset,
		statefulSetLister:  statefulSetInformer.Lister(),
		statefulSetsSynced: statefulSetInformer.Informer().HasSynced,
		pvcLister:          pvcInformer.Lister(),
		pvcsSynched:        pvcInformer.Informer().HasSynced,
		podLister:          podInformer.Lister(),
		podsSynced:         podInformer.Informer().HasSynced,
		workqueue:          workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "StatefulSets"),
		recorder:           recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	statefulSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueStatefulSet,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueStatefulSet(new)
		},
	})
	// Set up an event handler for when Pod resources change. This
	// handler will lookup the owner of the given Pod, and if it is
	// owned by a StatefulSet resource will enqueue that StatefulSet
	// resource for processing. This way, we don't need to implement
	// custom logic for handling Pod resources. More info on this pattern:
	// https://github.com/kubernetes/community/blob/8cafef897a22026d42f5e5bb3f104febe7e29830/contributors/devel/controllers.md
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handlePod,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				// Periodic resync will send update events for all known Pods.
				// Two different versions of the same Pod will always have different RVs.
				return
			}
			controller.handlePod(newPod)
		},
		DeleteFunc: controller.handlePod,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting StatefulSet scaledown cleanup controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.statefulSetsSynced, c.podsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.V(4).Infof("Successfully processed '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the StatefulSet resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {

	glog.V(4).Infof("--------------------------------------------------------------------")
	glog.V(4).Infof("SyncHandler invoked for %s", key)

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the StatefulSet resource with this namespace/name
	sts, err := c.statefulSetLister.StatefulSets(namespace).Get(name)
	if err != nil {
		// The StatefulSet may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("StatefulSet '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	return c.processStatefulSet(sts)
}

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
			err = c.cleanUpDrainPodIfNeeded(sts, pod, ordinal)
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

func (c *Controller) cleanUpDrainPodIfNeeded(sts *appsv1.StatefulSet, pod *corev1.Pod, ordinal int) error {
	// Drain Pod already exists. Check if it's done draining.
	if pod.Status.Phase == corev1.PodSucceeded {
		podName := getPodName(sts, ordinal)

		glog.Infof("Drain pod '%s' finished.", podName)
		c.recorder.Event(sts, corev1.EventTypeNormal, DrainSuccess, fmt.Sprintf(MessageDrainPodFinished, podName, sts.Name))

		for _, pvcTemplate := range sts.Spec.VolumeClaimTemplates {
			pvcName := getPVCName(sts, pvcTemplate.Name, int32(ordinal))
			glog.Infof("Deleting PVC %s", pvcName)
			err := c.kubeclientset.CoreV1().PersistentVolumeClaims(sts.Namespace).Delete(pvcName, nil)
			if err != nil {
				return err
			}
			c.recorder.Event(sts, corev1.EventTypeNormal, PVCDeleteSuccess, fmt.Sprintf(MessagePVCDeleted, pvcName, sts.Name))
		}

		// TODO what if the user scales up the statefulset and the statefulset controller creates the new pod after we delete the pod but before we delete the PVC
		// TODO what if we crash after we delete the PVC, but before we delete the pod?

		glog.Infof("Deleting drain pod %s", podName)
		err := c.kubeclientset.CoreV1().Pods(sts.Namespace).Delete(podName, nil)
		if err != nil {
			return err
		}
		c.recorder.Event(sts, corev1.EventTypeNormal, PodDeleteSuccess, fmt.Sprintf(MessageDrainPodDeleted, podName, sts.Name))
	}
	return nil
}

func isDrainPod(pod *corev1.Pod) bool {
	return pod != nil && pod.ObjectMeta.Annotations[AnnotationStatefulSet] != ""
}

// enqueueStatefulSet takes a StatefulSet resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than StatefulSet.
func (c *Controller) enqueueStatefulSet(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handlePod will take any resource implementing metav1.Object and attempt
// to find the StatefulSet resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that StatefulSet resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handlePod(obj interface{}) {

	if !c.cachesSynced() {
		return
	}

	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(5).Infof("Processing object: %s", object.GetName())

	stsNameFromAnnotation := object.GetAnnotations()[AnnotationStatefulSet]
	if stsNameFromAnnotation != "" {
		glog.V(5).Infof("Found pod with '%s' annotation pointing to StatefulSet '%s'. Enqueueing StatefulSet.", AnnotationStatefulSet, stsNameFromAnnotation)
		sts, err := c.statefulSetLister.StatefulSets(object.GetNamespace()).Get(stsNameFromAnnotation)
		if err != nil {
			glog.V(4).Infof("Error retrieving StatefulSet %s: %s", stsNameFromAnnotation, err)
			return
		}

		c.enqueueStatefulSet(sts)
		return
	}

	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a StatefulSet, we should not do anything more
		// with it.
		if ownerRef.Kind != "StatefulSet" {
			return
		}

		sts, err := c.statefulSetLister.StatefulSets(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			glog.V(4).Infof("ignoring orphaned object '%s' of StatefulSet '%s'", object.GetSelfLink(), ownerRef.Name)
			return
		}

		c.enqueueStatefulSet(sts)
		return
	}
}

func (c *Controller) cachesSynced() bool {
	return true; // TODO do we even need this?
}

func newPod(sts *appsv1.StatefulSet, ordinal int) (*corev1.Pod, error) {

	podTemplateJson := sts.Annotations[AnnotationDrainerPodTemplate]
	if podTemplateJson == "" {
		return nil, fmt.Errorf("No drain pod template configured for StatefulSet %s.", sts.Name)
	}
	pod := corev1.Pod{}
	err := json.Unmarshal([]byte(podTemplateJson), &pod)
	if err != nil {
		return nil, fmt.Errorf("Can't unmarshal DrainerPodTemplate JSON from annotation: %s", err)
	}

	pod.Name = getPodName(sts, ordinal)
	pod.Namespace = sts.Namespace

	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelDrainPod] = pod.Name;
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationStatefulSet] = sts.Name

	// TODO: cannot set blockOwnerDeletion if an ownerReference refers to a resource you can't set finalizers on: User "system:serviceaccount:kube-system:statefulset-drain-controller" cannot update statefulsets/finalizers.apps
	//if pod.OwnerReferences == nil {
	//	pod.OwnerReferences = []metav1.OwnerReference{}
	//}
	//pod.OwnerReferences = append(pod.OwnerReferences, *metav1.NewControllerRef(sts, schema.GroupVersionKind{
	//	Group:   appsv1beta1.SchemeGroupVersion.Group,
	//	Version: appsv1beta1.SchemeGroupVersion.Version,
	//	Kind:    "StatefulSet",
	//}))

	pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure

	for _, pvcTemplate := range sts.Spec.VolumeClaimTemplates {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{ // TODO: override existing volumes with the same name
			Name: pvcTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: getPVCName(sts, pvcTemplate.Name, int32(ordinal)),
				},
			},
		})
	}

	return &pod, nil
}

func getPodName(sts *appsv1.StatefulSet, ordinal int) string {
	return fmt.Sprintf("%s-%d", sts.Name, ordinal)
}

func getPVCName(sts *appsv1.StatefulSet, volumeClaimName string, ordinal int32) string {
	return fmt.Sprintf("%s-%s-%d", volumeClaimName, sts.Name, ordinal)
}

func extractNameAndOrdinal(pvcName string) (string, int, error) {
	idx := strings.LastIndexAny(pvcName, "-")
	if idx == -1 {
		return "", 0, fmt.Errorf("PVC not created by a StatefulSet")
	}

	name := pvcName[:idx]
	ordinal, err := strconv.Atoi(pvcName[idx+1:])
	if err != nil {
		return "", 0, fmt.Errorf("PVC not created by a StatefulSet")
	}
	return name, ordinal, nil
}
