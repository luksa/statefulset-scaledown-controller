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

package main

import (
	"flag"
	"time"

	"github.com/golang/glog"
	"github.com/jboss-openshift/statefulset-scaledown-controller/pkg/controller"
	"github.com/jboss-openshift/statefulset-scaledown-controller/pkg/signals"
	"io/ioutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"os"
)

type ConfigOptions struct {
	masterURL  string
	kubeconfig string
	namespace  string
	localOnly  bool

	LeaderElection          LeaderElectionConfiguration
	LeaderElectionNamespace string
}

const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// LeaderElectionConfiguration defines the configuration of leader election
// clients for components that can run with leader election enabled.
type LeaderElectionConfiguration struct {
	// leaderElect enables a leader election client to gain leadership
	// before executing the main loop. Enable this when running replicated
	// components for high availability.
	LeaderElect bool
	// leaseDuration is the duration that non-leader candidates will wait
	// after observing a leadership renewal until attempting to acquire
	// leadership of a led but unrenewed leader slot. This is effectively the
	// maximum duration that a leader can be stopped before it is replaced
	// by another candidate. This is only applicable if leader election is
	// enabled.
	LeaseDuration metav1.Duration
	// renewDeadline is the interval between attempts by the acting master to
	// renew a leadership slot before it stops leading. This must be less
	// than or equal to the lease duration. This is only applicable if leader
	// election is enabled.
	RenewDeadline metav1.Duration
	// retryPeriod is the duration the clients should wait between attempting
	// acquisition and renewal of a leadership. This is only applicable if
	// leader election is enabled.
	RetryPeriod metav1.Duration
	// resourceLock indicates the resource object type that will be used to lock
	// during leader election cycles.
	ResourceLock string
}

var config ConfigOptions

func main() {
	flag.Parse()

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(config.masterURL, config.kubeconfig)
	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	var kubeInformerFactory kubeinformers.SharedInformerFactory
	if config.localOnly {
		if config.namespace == "" {
			bytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
			if err != nil {
				glog.Fatalf("Using --localOnly without --namespace, but unable to determine namespace: %s", err.Error())
			}
			config.namespace = string(bytes)
		}

		glog.Infof("Configured to only operate on StatefulSets in namespace %s", config.namespace)
		kubeInformerFactory = kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*30, config.namespace, nil)
	} else {
		glog.Infof("Configured to operate on StatefulSets across all namespaces")
		kubeInformerFactory = kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	}

	c := controller.NewController(kubeClient, kubeInformerFactory)

	runController := func(stopCh <-chan struct{}) {
		go kubeInformerFactory.Start(stopCh)

		if err = c.Run(1, stopCh); err != nil {
			glog.Fatalf("Error running controller: %s", err.Error())
		}
	}

	if !config.LeaderElection.LeaderElect {
		glog.Infof("Running without leader election (non-HA mode)")
		runController(stopCh)
		panic("unreachable")
	}

	glog.Infof("Running with leader election enabled (HA mode)")
	// Identity used to distinguish between multiple cloud controller manager instances
	id, err := os.Hostname()
	if err != nil {
		glog.Fatalf("Can't determine hostname: %s", err.Error())
	}

	glog.V(5).Infof("Using namespace %v for leader election lock", config.LeaderElectionNamespace)

	leaderElectionClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(cfg, "leader-election"))

	// Lock required for leader election
	lock, err := resourcelock.New(
		config.LeaderElection.ResourceLock,
		config.LeaderElectionNamespace,
		"statefulset-scaledown-controller",
		leaderElectionClient.CoreV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id + "-external-statefulset-scaledown-controller",
			EventRecorder: c.Recorder,
		})
	if err != nil {
		glog.Fatalf("Can't create resource lock: %s", err.Error())
	}

	// Try and become the leader and start controller loop
	leaderelection.RunOrDie(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: config.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: config.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   config.LeaderElection.RetryPeriod.Duration,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: runController,
			OnStoppedLeading: func() {
				glog.Fatalf("Stopped being the leader. Exiting!")
			},
		},
	})
	panic("unreachable")

}

func init() {
	flag.StringVar(&config.kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&config.masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.BoolVar(&config.localOnly, "localOnly", false, "If enabled, the controller only handles StatefulSets in a single namespace instead of across all namespaces.")
	flag.StringVar(&config.namespace, "namespace", "", "Only used with --localOnly. When specified, the controller only handles StatefulSets in the specified namespace. If left empty, the controller defaults to the namespace it is running in (read from /var/run/secrets/kubernetes.io/serviceaccount/namespace).")

	flag.BoolVar(&config.LeaderElection.LeaderElect, "leader-elect", false, ""+
		"Start a leader election client and gain leadership before "+
		"executing the main loop. Enable this when running replicated "+
		"components for high availability.")
	flag.DurationVar(&config.LeaderElection.LeaseDuration.Duration, "leader-elect-lease-duration", DefaultLeaseDuration, ""+
		"The duration that non-leader candidates will wait after observing a leadership "+
		"renewal until attempting to acquire leadership of a led but unrenewed leader "+
		"slot. This is effectively the maximum duration that a leader can be stopped "+
		"before it is replaced by another candidate. This is only applicable if leader "+
		"election is enabled.")
	flag.DurationVar(&config.LeaderElection.RenewDeadline.Duration, "leader-elect-renew-deadline", DefaultRenewDeadline, ""+
		"The interval between attempts by the acting master to renew a leadership slot "+
		"before it stops leading. This must be less than or equal to the lease duration. "+
		"This is only applicable if leader election is enabled.")
	flag.DurationVar(&config.LeaderElection.RetryPeriod.Duration, "leader-elect-retry-period", DefaultRetryPeriod, ""+
		"The duration the clients should wait between attempting acquisition and renewal "+
		"of a leadership. This is only applicable if leader election is enabled.")
	flag.StringVar(&config.LeaderElection.ResourceLock, "leader-elect-resource-lock", resourcelock.ConfigMapsResourceLock, ""+
		"The type of resource object that is used for locking during "+
		"leader election. Supported options are `configmaps` (default) and `endpoints`.")

	flag.StringVar(&config.LeaderElectionNamespace, "leader-election-namespace", "", "Namespace to use for leader election lock (defaults to the controller's namespace)")
}
