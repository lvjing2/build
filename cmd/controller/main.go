/*
Copyright 2018 The Knative Authors

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
	"log"
	"time"

	"go.uber.org/zap"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	onclusterbuilder "github.com/knative/build/pkg/builder/cluster"
	"github.com/knative/build/pkg/controller"
	buildctrl "github.com/knative/build/pkg/controller/build"
	"github.com/knative/build/pkg/reconciler/build"
	"github.com/knative/build/pkg/reconciler/buildtemplate"
	"github.com/knative/build/pkg/reconciler/clusterbuildtemplate"

	buildclientset "github.com/knative/build/pkg/client/clientset/versioned"
	informers "github.com/knative/build/pkg/client/informers/externalversions"
	cachingclientset "github.com/knative/caching/pkg/client/clientset/versioned"
	cachinginformers "github.com/knative/caching/pkg/client/informers/externalversions"
	"github.com/knative/pkg/configmap"
	"github.com/knative/pkg/logging"
	"github.com/knative/pkg/logging/logkey"
	"github.com/knative/pkg/signals"
)

const (
	threadsPerController = 2
	logLevelKey          = "controller"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	masterURL  = flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
)

func main() {
	flag.Parse()
	loggingConfigMap, err := configmap.Load("/etc/config-logging")
	if err != nil {
		log.Fatalf("Error loading logging configuration: %v", err)
	}
	loggingConfig, err := logging.NewConfigFromMap(loggingConfigMap)
	if err != nil {
		log.Fatalf("Error parsing logging configuration: %v", err)
	}
	logger, _ := logging.NewLoggerFromConfig(loggingConfig, logLevelKey)
	defer logger.Sync()
	logger = logger.With(zap.String(logkey.ControllerType, logLevelKey))

	logger.Info("Starting the Build Controller")

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfig)
	if err != nil {
		logger.Fatalf("Error building kubeconfig: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building kubernetes clientset: %v", err)
	}

	buildClient, err := buildclientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building Build clientset: %v", err)
	}

	cachingClient, err := cachingclientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building Caching clientset: %v", err)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	buildInformerFactory := informers.NewSharedInformerFactory(buildClient, time.Second*30)
	cachingInformerFactory := cachinginformers.NewSharedInformerFactory(cachingClient, time.Second*30)

	buildInformer := buildInformerFactory.Build().V1alpha1().Builds()
	buildTemplateInformer := buildInformerFactory.Build().V1alpha1().BuildTemplates()
	clusterBuildTemplateInformer := buildInformerFactory.Build().V1alpha1().ClusterBuildTemplates()
	imageInformer := cachingInformerFactory.Caching().V1alpha1().Images()

	bldr := onclusterbuilder.NewBuilder(kubeClient, kubeInformerFactory, logger)

	// Build all of our controllers, with the clients constructed above.
	controllers := []controller.Interface{
		// TODO(mattmoor): Move the Build controller logic into pkg/reconciler/build
		buildctrl.NewController(bldr, kubeClient, buildClient,
			kubeInformerFactory, buildInformerFactory, logger),

		build.NewController(logger, kubeClient, buildClient, buildInformer),
		clusterbuildtemplate.NewController(logger, kubeClient, buildClient,
			cachingClient, clusterBuildTemplateInformer, imageInformer),
		buildtemplate.NewController(logger, kubeClient, buildClient,
			cachingClient, buildTemplateInformer, imageInformer),
	}

	go kubeInformerFactory.Start(stopCh)
	go buildInformerFactory.Start(stopCh)
	go cachingInformerFactory.Start(stopCh)

	for i, synced := range []cache.InformerSynced{
		buildInformer.Informer().HasSynced,
		buildTemplateInformer.Informer().HasSynced,
		clusterBuildTemplateInformer.Informer().HasSynced,
		imageInformer.Informer().HasSynced,
	} {
		if ok := cache.WaitForCacheSync(stopCh, synced); !ok {
			logger.Fatalf("failed to wait for cache at index %v to sync", i)
		}
	}

	// Start all of the controllers.
	for _, ctrlr := range controllers {
		go func(ctrlr controller.Interface) {
			// We don't expect this to return until stop is called,
			// but if it does, propagate it back.
			if err := ctrlr.Run(threadsPerController, stopCh); err != nil {
				logger.Fatalf("Error running controller: %s", err.Error())
			}
		}(ctrlr)
	}

	// TODO(mattmoor): Use a sync.WaitGroup instead?
	<-stopCh
}
