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

package apibinding

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	kcpcache "github.com/kcp-dev/apimachinery/v2/pkg/cache"
	kcpdynamic "github.com/kcp-dev/client-go/dynamic"
	"github.com/kcp-dev/logicalcluster/v3"

	"k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kcpapiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/kcp/clientset/versioned"
	kcpapiextensionsv1informers "k8s.io/apiextensions-apiserver/pkg/client/kcp/informers/externalversions/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/apis/core"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/cluster"
	apisv1alpha1client "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/typed/apis/v1alpha1"
	apisv1alpha1informers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/apis/v1alpha1"
	apisv1alpha1listers "github.com/kcp-dev/kcp/pkg/client/listers/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/indexers"
	"github.com/kcp-dev/kcp/pkg/informer"
	"github.com/kcp-dev/kcp/pkg/logging"
	"github.com/kcp-dev/kcp/pkg/reconciler/committer"
)

const (
	ControllerName = "kcp-apibinding"
)

var (
	SystemBoundCRDsClusterName = logicalcluster.Name("system:bound-crds")
)

// NewController returns a new controller for APIBindings.
func NewController(
	crdClusterClient kcpapiextensionsclientset.ClusterInterface,
	kcpClusterClient kcpclientset.ClusterInterface,
	dynamicClusterClient kcpdynamic.ClusterInterface,
	dynamicDiscoverySharedInformerFactory *informer.DiscoveringDynamicSharedInformerFactory,
	apiBindingInformer apisv1alpha1informers.APIBindingClusterInformer,
	apiExportInformer apisv1alpha1informers.APIExportClusterInformer,
	apiResourceSchemaInformer apisv1alpha1informers.APIResourceSchemaClusterInformer,
	globalAPIExportInformer apisv1alpha1informers.APIExportClusterInformer,
	globalAPIResourceSchemaInformer apisv1alpha1informers.APIResourceSchemaClusterInformer,
	crdInformer kcpapiextensionsv1informers.CustomResourceDefinitionClusterInformer,
) (*controller, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName)

	c := &controller{
		queue:                queue,
		crdClusterClient:     crdClusterClient,
		kcpClusterClient:     kcpClusterClient,
		dynamicClusterClient: dynamicClusterClient,
		ddsif:                dynamicDiscoverySharedInformerFactory,

		apiBindingsLister: apiBindingInformer.Lister(),
		listAPIBindings: func(clusterName logicalcluster.Name) ([]*apisv1alpha1.APIBinding, error) {
			list, err := apiBindingInformer.Lister().List(labels.Everything())
			if err != nil {
				return nil, err
			}

			var ret []*apisv1alpha1.APIBinding

			for i := range list {
				if logicalcluster.From(list[i]) != clusterName {
					continue
				}

				ret = append(ret, list[i])
			}

			return ret, nil
		},
		apiBindingsIndexer: apiBindingInformer.Informer().GetIndexer(),

		getAPIExport: func(path logicalcluster.Path, name string) (*apisv1alpha1.APIExport, error) {
			// Try local informer first
			export, err := indexers.ByPathAndName[*apisv1alpha1.APIExport](apisv1alpha1.Resource("apiexports"), apiExportInformer.Informer().GetIndexer(), path, name)
			if err == nil {
				// Quick happy path - found it locally
				return export, nil
			}
			if !apierrors.IsNotFound(err) {
				// Unrecoverable error
				return nil, err
			}
			// Didn't find it locally - try remote
			return indexers.ByPathAndName[*apisv1alpha1.APIExport](apisv1alpha1.Resource("apiexports"), globalAPIExportInformer.Informer().GetIndexer(), path, name)
		},
		apiExportsIndexer:       apiExportInformer.Informer().GetIndexer(),
		globalAPIExportsIndexer: globalAPIExportInformer.Informer().GetIndexer(),

		getAPIResourceSchema: func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIResourceSchema, error) {
			apiResourceSchema, err := apiResourceSchemaInformer.Lister().Cluster(clusterName).Get(name)
			if apierrors.IsNotFound(err) {
				return globalAPIResourceSchemaInformer.Lister().Cluster(clusterName).Get(name)
			}
			return apiResourceSchema, err
		},

		createCRD: func(ctx context.Context, clusterName logicalcluster.Path, crd *apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error) {
			return crdClusterClient.Cluster(clusterName).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		},
		getCRD: func(clusterName logicalcluster.Name, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
			return crdInformer.Lister().Cluster(clusterName).Get(name)
		},
		listCRDs: func(clusterName logicalcluster.Name) ([]*apiextensionsv1.CustomResourceDefinition, error) {
			return crdInformer.Lister().Cluster(clusterName).List(labels.Everything())
		},
		deletedCRDTracker: newLockedStringSet(),
		commit:            committer.NewCommitter[*APIBinding, Patcher, *APIBindingSpec, *APIBindingStatus](kcpClusterClient.ApisV1alpha1().APIBindings()),
	}

	logger := logging.WithReconciler(klog.Background(), ControllerName)

	if err := apiBindingInformer.Informer().AddIndexers(cache.Indexers{
		indexers.APIBindingsByAPIExport: indexers.IndexAPIBindingByAPIExport,
	}); err != nil {
		return nil, err
	}

	indexers.AddIfNotPresentOrDie(apiExportInformer.Informer().GetIndexer(), cache.Indexers{
		indexers.ByLogicalClusterPathAndName: indexers.IndexByLogicalClusterPathAndName,
		indexAPIExportsByAPIResourceSchema:   indexAPIExportsByAPIResourceSchemasFunc,
	})
	indexers.AddIfNotPresentOrDie(globalAPIExportInformer.Informer().GetIndexer(), cache.Indexers{
		indexers.ByLogicalClusterPathAndName: indexers.IndexByLogicalClusterPathAndName,
		indexAPIExportsByAPIResourceSchema:   indexAPIExportsByAPIResourceSchemasFunc,
	})

	apiBindingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIBinding(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIBinding(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIBinding(obj, logger, "") },
	})

	crdInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return false
			}

			return logicalcluster.From(crd) == SystemBoundCRDsClusterName
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.enqueueCRD(obj, logger) },
			UpdateFunc: func(_, obj interface{}) { c.enqueueCRD(obj, logger) },
			DeleteFunc: func(obj interface{}) {
				meta, err := meta.Accessor(obj)
				if err != nil {
					runtime.HandleError(err)
					return
				}

				// If something deletes one of our bound CRDs, we need to keep track of it so when we're reconciling,
				// we know we need to recreate it. This set is there to fight against stale informers still seeing
				// the deleted CRD.
				c.deletedCRDTracker.Add(meta.GetName())

				c.enqueueCRD(obj, logger)
			},
		},
	})

	apiResourceSchemaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
	})

	apiExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
	})
	globalAPIExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
	})
	globalAPIResourceSchemaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
	})

	return c, nil
}

type APIBinding = apisv1alpha1.APIBinding
type APIBindingSpec = apisv1alpha1.APIBindingSpec
type APIBindingStatus = apisv1alpha1.APIBindingStatus
type Patcher = apisv1alpha1client.APIBindingInterface
type Resource = committer.Resource[*APIBindingSpec, *APIBindingStatus]
type CommitFunc = func(context.Context, *Resource, *Resource) error

// controller reconciles APIBindings. It creates and maintains CRDs associated with APIResourceSchemas that are
// referenced from APIBindings. It also watches CRDs, APIResourceSchemas, and APIExports to ensure whenever
// objects related to an APIBinding are updated, the APIBinding is reconciled.
type controller struct {
	queue workqueue.RateLimitingInterface

	crdClusterClient     kcpapiextensionsclientset.ClusterInterface
	kcpClusterClient     kcpclientset.ClusterInterface
	dynamicClusterClient kcpdynamic.ClusterInterface
	ddsif                *informer.DiscoveringDynamicSharedInformerFactory

	apiBindingsLister  apisv1alpha1listers.APIBindingClusterLister
	listAPIBindings    func(clusterName logicalcluster.Name) ([]*apisv1alpha1.APIBinding, error)
	apiBindingsIndexer cache.Indexer

	getAPIExport            func(path logicalcluster.Path, name string) (*apisv1alpha1.APIExport, error)
	apiExportsIndexer       cache.Indexer
	globalAPIExportsIndexer cache.Indexer

	getAPIResourceSchema func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIResourceSchema, error)

	createCRD func(ctx context.Context, clusterName logicalcluster.Path, crd *apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error)
	getCRD    func(clusterName logicalcluster.Name, name string) (*apiextensionsv1.CustomResourceDefinition, error)
	listCRDs  func(clusterName logicalcluster.Name) ([]*apiextensionsv1.CustomResourceDefinition, error)

	deletedCRDTracker *lockedStringSet
	commit            CommitFunc
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
func (c *controller) enqueueAPIExport(obj interface{}, logger logr.Logger, logSuffix string) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}

	export, ok := obj.(*apisv1alpha1.APIExport)
	if !ok {
		runtime.HandleError(fmt.Errorf("obj is supposed to be a APIExport, but is %T", obj))
		return
	}

	// binding keys by full path
	keys := sets.NewString()
	if path := logicalcluster.NewPath(export.Annotations[core.LogicalClusterPathAnnotationKey]); !path.Empty() {
		pathKeys, err := c.apiBindingsIndexer.IndexKeys(indexers.APIBindingsByAPIExport, path.Join(export.Name).String())
		if err != nil {
			runtime.HandleError(err)
			return
		}
		keys.Insert(pathKeys...)
	}

	clusterKeys, err := c.apiBindingsIndexer.IndexKeys(indexers.APIBindingsByAPIExport, logicalcluster.From(export).Path().Join(export.Name).String())
	if err != nil {
		runtime.HandleError(err)
		return
	}
	keys.Insert(clusterKeys...)

	for _, key := range keys.List() {
		binding, exists, err := c.apiBindingsIndexer.GetByKey(key)
		if err != nil {
			runtime.HandleError(err)
			continue
		} else if !exists {
			runtime.HandleError(fmt.Errorf("APIBinding %q does not exist", key))
			continue
		}
		c.enqueueAPIBinding(binding, logging.WithObject(logger, obj.(*apisv1alpha1.APIExport)), fmt.Sprintf(" because of APIExport%s", logSuffix))
	}
}

// enqueueCRD maps a CRD to APIResourceSchema for enqueuing.
func (c *controller) enqueueCRD(obj interface{}, logger logr.Logger) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		runtime.HandleError(fmt.Errorf("obj is supposed to be a CustomResourceDefinition, but is %T", obj))
		return
	}
	logger = logging.WithObject(logger, crd).WithValues(
		"groupResource", fmt.Sprintf("%s.%s", crd.Spec.Names.Plural, crd.Spec.Group),
		"established", apihelpers.IsCRDConditionTrue(crd, apiextensionsv1.Established),
	)

	if crd.Annotations[apisv1alpha1.AnnotationSchemaClusterKey] == "" || crd.Annotations[apisv1alpha1.AnnotationSchemaNameKey] == "" {
		logger.V(4).Info("skipping CRD because does not belong to an APIResourceSchema")
		return
	}

	clusterName := logicalcluster.Name(crd.Annotations[apisv1alpha1.AnnotationSchemaClusterKey])
	apiResourceSchema, err := c.getAPIResourceSchema(clusterName, crd.Annotations[apisv1alpha1.AnnotationSchemaNameKey])
	if err != nil {
		runtime.HandleError(err)
		return
	}

	// this log here is kind of redundant normally. But we are seeing missing CRD update events
	// and hence stale APIBindings. So this might help to undersand what's going on.
	logger.V(4).Info("queueing APIResourceSchema because of CRD", "key", kcpcache.ToClusterAwareKey(clusterName.String(), "", apiResourceSchema.Name))

	c.enqueueAPIResourceSchema(apiResourceSchema, logger, " because of CRD")
}

// enqueueAPIResourceSchema maps an APIResourceSchema to APIExports for enqueuing.
func (c *controller) enqueueAPIResourceSchema(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := kcpcache.DeletionHandlingMetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	apiExports, err := c.apiExportsIndexer.ByIndex(indexAPIExportsByAPIResourceSchema, key)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	if len(apiExports) == 0 {
		apiExports, err = c.globalAPIExportsIndexer.ByIndex(indexAPIExportsByAPIResourceSchema, key)
		if err != nil {
			runtime.HandleError(err)
			return
		}
	}

	for _, export := range apiExports {
		c.enqueueAPIExport(export, logging.WithObject(logger, obj.(*apisv1alpha1.APIResourceSchema)), fmt.Sprintf(" because of APIResourceSchema%s", logSuffix))
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

	if requeue, err := c.process(ctx, key); err != nil {
		runtime.HandleError(fmt.Errorf("%q controller failed to sync %q, err: %w", ControllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	} else if requeue {
		// only requeue if we didn't error, but we still want to requeue
		c.queue.Add(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

func (c *controller) process(ctx context.Context, key string) (bool, error) {
	logger := klog.FromContext(ctx)
	clusterName, _, name, err := kcpcache.SplitMetaClusterNamespaceKey(key)
	if err != nil {
		runtime.HandleError(err)
		return false, nil
	}

	obj, err := c.apiBindingsLister.Cluster(clusterName).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "failed to get APIBinding from lister", "cluster", clusterName)
		}

		return false, nil // nothing we can do here
	}

	old := obj
	obj = obj.DeepCopy()

	logger = logging.WithObject(logger, obj)
	ctx = klog.NewContext(ctx, logger)

	var errs []error
	requeue, err := c.reconcile(ctx, obj)
	if err != nil {
		errs = append(errs, err)
	}

	// If the object being reconciled changed as a result, update it.
	oldResource := &Resource{ObjectMeta: old.ObjectMeta, Spec: &old.Spec, Status: &old.Status}
	newResource := &Resource{ObjectMeta: obj.ObjectMeta, Spec: &obj.Spec, Status: &obj.Status}
	if err := c.commit(ctx, oldResource, newResource); err != nil {
		errs = append(errs, err)
	}

	return requeue, utilerrors.NewAggregate(errs)
}
