/*
Copyright 2022 The KCP Authors.

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

package apibindinglabel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	kcpcache "github.com/kcp-dev/apimachinery/pkg/cache"
	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/cluster"
	apisinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/apis/v1alpha1"
	apislisters "github.com/kcp-dev/kcp/pkg/client/listers/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/indexers"
	"github.com/kcp-dev/kcp/pkg/logging"
	"github.com/kcp-dev/kcp/pkg/reconciler/committer"
)

const (
	ControllerName = "kcp-workload-apibinding-labelsync"
)

// NewController returns a new controller instance.
func NewController(
	kcpClusterClient kcpclientset.ClusterInterface,
	apiExportInformer apisinformers.APIExportClusterInformer,
	apiBindingInformer apisinformers.APIBindingClusterInformer,
) (*controller, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName)

	c := &controller{
		queue: queue,

		kcpClusterClient: kcpClusterClient,

		apiExportsLister: apiExportInformer.Lister(),

		apiBindingsLister: apiBindingInformer.Lister(),

		getAPIBindingsByAPIExportKey: func(key string) ([]*apisv1alpha1.APIBinding, error) {
			return indexers.ByIndex[*apisv1alpha1.APIBinding](apiBindingInformer.Informer().GetIndexer(), indexers.APIBindingsByAPIExport, key)
		},
	}

	logger := logging.WithReconciler(klog.Background(), ControllerName)

	apiExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger) },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger) },
	})

	apiBindingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIBinding(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIBinding(obj, logger, "") },
	})

	return c, nil
}

type Resource = committer.Resource[*apisv1alpha1.APIBindingSpec, *apisv1alpha1.APIBindingStatus]
type CommitFunc = func(context.Context, *Resource, *Resource) error

// controller reconciles sync workload.kcp.dev/compute label from APIExports to APIBindings
type controller struct {
	queue workqueue.RateLimitingInterface

	kcpClusterClient kcpclientset.ClusterInterface

	apiExportsLister apislisters.APIExportClusterLister

	apiBindingsLister apislisters.APIBindingClusterLister

	getAPIBindingsByAPIExportKey func(key string) ([]*apisv1alpha1.APIBinding, error)
}

// enqueueAPIBinding enqueues an APIBinding .
func (c *controller) enqueueAPIBinding(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := kcpcache.DeletionHandlingMetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	logging.WithQueueKey(logger, key).V(2).Info(fmt.Sprintf("queueing APIBinding%s", logSuffix))
	c.queue.Add(key)
}

// enqueueAPIExport enqueues maps an APIExport to APIBindings for enqueuing.
func (c *controller) enqueueAPIExport(obj interface{}, logger logr.Logger) {
	key, err := kcpcache.DeletionHandlingMetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	bindingsForExport, err := c.getAPIBindingsByAPIExportKey(key)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	for _, binding := range bindingsForExport {
		c.enqueueAPIBinding(binding, logging.WithObject(logger, obj.(*apisv1alpha1.APIExport)), " because of APIExport")
	}
}

// Start starts the controller, which stops when ctx.Done() is closed.
func (c *controller) Start(ctx context.Context, numThreads int) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	logger := logging.WithReconciler(klog.FromContext(ctx), ControllerName)
	ctx = klog.NewContext(ctx, logger)
	logger.Info("Starting controller")
	defer logger.Info("Shutting down controller")

	for i := 0; i < numThreads; i++ {
		go wait.UntilWithContext(ctx, c.startWorker, time.Second)
	}

	<-ctx.Done()
}

func (c *controller) startWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *controller) processNextWorkItem(ctx context.Context) bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	logger := logging.WithQueueKey(klog.FromContext(ctx), key)
	ctx = klog.NewContext(ctx, logger)
	logger.V(1).Info("processing key")

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	if err := c.process(ctx, key); err != nil {
		runtime.HandleError(fmt.Errorf("%q controller failed to sync %q, err: %w", ControllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

func (c *controller) process(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	clusterName, _, name, err := kcpcache.SplitMetaClusterNamespaceKey(key)
	if err != nil {
		logger.Error(err, "invalid key")
		return nil
	}
	apiBinding, err := c.apiBindingsLister.Cluster(clusterName).Get(name)
	if errors.IsNotFound(err) {
		return nil // object deleted before we handled it
	}
	if err != nil {
		return err
	}

	if apiBinding.Spec.Reference.Workspace == nil {
		return nil
	}

	apiExportClusterName := logicalcluster.New(apiBinding.Spec.Reference.Workspace.Path)
	apiExport, err := c.apiExportsLister.Cluster(apiExportClusterName).Get(apiBinding.Spec.Reference.Workspace.ExportName)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	labels := map[string]interface{}{} // nil means to remove the key
	var apiExportHasLabel, apiBindingHasLabel bool
	if _, ok := apiExport.Labels[workloadv1alpha1.ComputeAPIBindingLabel]; ok {
		apiExportHasLabel = true
	}
	if _, ok := apiBinding.Labels[workloadv1alpha1.ComputeAPIBindingLabel]; ok {
		apiBindingHasLabel = true
	}

	if apiExportHasLabel && !apiBindingHasLabel {
		labels[workloadv1alpha1.ComputeAPIBindingLabel] = ""
	} else if !apiExportHasLabel && apiBindingHasLabel {
		labels[workloadv1alpha1.ComputeAPIBindingLabel] = nil
	}

	if len(labels) == 0 {
		return nil
	}

	patch := map[string]interface{}{}
	if err := unstructured.SetNestedField(patch, labels, "metadata", "labels"); err != nil {
		return err
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = c.kcpClusterClient.Cluster(clusterName).SchedulingV1alpha1().Placements().Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}
