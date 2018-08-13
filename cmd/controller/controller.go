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
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	masterURL  string
	kubeconfig string
	namespace  string
	localOnly  bool
)

func main() {
	flag.Parse()

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	var kubeInformerFactory kubeinformers.SharedInformerFactory
	if localOnly {
		if namespace == "" {
			bytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
			if err != nil {
				glog.Fatalf("Using --localOnly without --namespace, but unable to determine namespace: %s", err.Error())
			}
			namespace = string(bytes)
		}

		glog.Infof("Configured to only operate on StatefulSets in namespace %s", namespace)
		kubeInformerFactory = kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*30, namespace, nil)
	} else {
		glog.Infof("Configured to operate on StatefulSets across all namespaces")
		kubeInformerFactory = kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	}

	c := controller.NewController(kubeClient, kubeInformerFactory)

	go kubeInformerFactory.Start(stopCh)

	if err = c.Run(1, stopCh); err != nil {
		glog.Fatalf("Error running controller: %s", err.Error())
	}
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.BoolVar(&localOnly, "localOnly", false, "If enabled, the controller only handles StatefulSets in a single namespace instead of across all namespaces.")
	flag.StringVar(&namespace, "namespace", "", "Only used with --localOnly. When specified, the controller only handles StatefulSets in the specified namespace. If left empty, the controller defaults to the namespace it is running in (read from /var/run/secrets/kubernetes.io/serviceaccount/namespace).")
}
