# StatefulSet Scale-Down Controller

This repository implements a controller for running user-defined cleanup jobs for
orphaned PersistentVolumeClaims/PersistentVolumes when a StatefulSet is scaled down.


## Purpose

When a StatefulSet is scaled down and one of the stateful pod instances is removed, the associated 
PersistentVolumeClaim (and the underlying PersistentVolume) are left intact. The data stored on 
the PersistentVolume remains inaccessible until the StatefulSet is scaled back up. In stateful 
apps that use sharding, that may not be ideal. Those apps require the data to be redistributed to
the remaining app instances. The StatefulSet Scale-Down Controller allows you to specify a cleanup
pod template in the StatefulSet spec, which will be used to create a new cleanup pod that
is attached to the PersistentVolume that was released by the scale down. The pod thus has access
to the data of the removed pod instance and can do whatever the app requires with it (e.g. 
redistribute it to the other instances or process it in a different way). Once the cleanup pod
completes, the controller deletes the pod and the PersistentVolumeClaim, releasing the 
PersistentVolume.

You can read more about the controller in [this blog post](https://medium.com/@marko.luksa/graceful-scaledown-of-stateful-apps-in-kubernetes-2205fc556ba9).

## Running
The controller can be run as:
- a cluster-level component:  you deploy only one controller for the whole cluster, but this 
  requires cluster-level privileges
- per-namespace: you deploy the controller in the same namespace as your stateful app(s) and the 
  controller will only operate on StatefulSets in that namespace. This mode doesn't require 
  cluster-level privileges.

The `artifacts/` directory in this repo contains YAML files for deploying the controller in either
of these modes.

### Running one controller for the whole cluster
```bash
$ kubectl apply -f https://raw.githubusercontent.com/jboss-openshift/statefulset-scaledown-controller/master/artifacts/cluster-scoped.yaml
```

### Running controller in a single namespace
```bash
$ kubectl apply -f https://raw.githubusercontent.com/jboss-openshift/statefulset-scaledown-controller/master/artifacts/per-namespace.yaml
```

## How to use
Once the controller is running and you've created a container image that knows how to process 
your data, you can add a cleanup pod template to your StatefulSet spec by adding the annotation 
`statefulsets.kubernetes.io/scaledown-pod-template` as shown in the example:

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: my-statefulset
  annotations:
    statefulsets.kubernetes.io/scaledown-pod-template: |
      {
        "metadata": {
          "labels": {
            "app": "datastore-cleanup"
          }
        },
        "spec": {
          "containers": [
            {
              "name": "cleanup",
              "image": "my-cleanup-container",
              "volumeMounts": [
                {
                  "name": "data",
                  "mountPath": "/var/data"
                }
              ]
            }
          ]
        }
      }
spec:
  ...
```

Every time you scale down the StatefulSet, the controller will create a pod from the template
specified in the annotation and attach it to the PersistentVolumeClaim that was previously 
attached to the stateful pod instance that was removed because of the scale down. 


## Example

To see the controller in action, you can deploy the example in `example/statefulset.yaml` and scale 
it down from 3 to 2 replicas. The `datastore-2` pod will terminate and then, if the controller is
working properly, you should see a new `datastore-2` pod spin up and do its work. Once it finishes,
the controller will delete the pod and the PersistentVolumeClaim.

```bash
$ kubectl apply -f https://raw.githubusercontent.com/jboss-openshift/statefulset-scaledown-controller/master/example/statefulset.yaml

... wait for all three pod instances to start running before scaling down

$ kubectl get po
NAME          READY     STATUS        RESTARTS   AGE
datastore-0   1/1       Running       0          3m
datastore-1   1/1       Running       0          2m
datastore-2   1/1       Running       0          5s

$ kubectl scale statefulset datastore --replicas 2
statefulset.apps/datastore scaled

$ kubectl get po
NAME          READY     STATUS        RESTARTS   AGE
datastore-0   1/1       Running       0          3m
datastore-1   1/1       Running       0          2m
datastore-2   1/1       Terminating   0          49s

$ kubectl get po
NAME          READY     STATUS    RESTARTS   AGE
datastore-0   1/1       Running   0          3m
datastore-1   1/1       Running   0          3m
datastore-2   1/1       Running   0          5s    <-- the cleanup pod

... wait approx. 10 seconds for the cleanup pod to finish

$ kubectl get po
NAME          READY     STATUS    RESTARTS   AGE
datastore-0   1/1       Running   0          3m
datastore-1   1/1       Running   0          3m

$ kubectl get pvc
NAME               STATUS    VOLUME             CAPACITY   ...
data-datastore-0   Bound     pvc-57224b8f-...   1Mi        ...
data-datastore-1   Bound     pvc-5acaf078-...   1Mi        ...
```

## Running in H/A mode

Multiple instances of the controller can be run in parallel to achieve high availability. 
By running the controller with the `--leader-elect` option, the controller only runs its control loop when it is the elected leader. 

The controller uses the standard Kubernetes mechanism for leader election and needs RBAC permissions to create, modify and retrieve a ConfigMap resource with the name `statefulset-scaledown-controller` in the namespace specified with `--leader-election-namespace`.


## Known issues / deficiencies

Please be aware of the following issues:
- If the StatefulSet is deleted, the controller currently does not create any cleanup pods. Please scale down the StatefulSet to zero, wait for all the cleanup pods to finish, and only then delete the StatefulSet.  
- If the StatefulSet is deleted while a cleanup pod is running, the pod is never deleted by the controller. Please delete the pod manually or scale downthe StatefulSet to zero before deleting it, as described above.
- Controller does not expose any metrics