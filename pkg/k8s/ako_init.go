/*
 * [2013] - [2019] Avi Networks Incorporated
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package k8s

import (
	"os"
	"strconv"
	"strings"
	"time"

	avicache "ako/pkg/cache"
	"ako/pkg/lib"
	"ako/pkg/nodes"
	"ako/pkg/objects"
	"ako/pkg/rest"
	"ako/pkg/retry"

	"github.com/avinetworks/container-lib/utils"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

func PopulateCache() {
	avi_rest_client_pool := avicache.SharedAVIClients()
	avi_obj_cache := avicache.SharedAviObjCache()
	// Randomly pickup a client.
	if len(avi_rest_client_pool.AviClient) > 0 {
		avi_obj_cache.AviObjCachePopulate(avi_rest_client_pool.AviClient[0],
			utils.CtrlVersion, utils.CloudName)
	}
}

func PopulateNodeCache(cs *kubernetes.Clientset) {
	nodeCache := objects.SharedNodeLister()
	nodeCache.PopulateAllNodes(cs)
}

// HandleConfigMap : initialise the controller, start informer for configmap and wait for the akc configmap to be created.
// When the configmap is created, enable sync for other k8s objects. When the configmap is disabled, disable sync.
func (c *AviController) HandleConfigMap(k8sinfo K8sinformers, ctrlCh chan struct{}, stopCh <-chan struct{}) {
	cs := k8sinfo.Cs
	utils.AviLog.Infof("Creating event broadcaster for handling configmap")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(utils.AviLog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	firstboot := true
	configMapEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm, ok := validateAviConfigMap(obj); ok {
				utils.AviLog.Infof("avi k8s configmap created")
				utils.AviLog.SetLevel(cm.Data[lib.LOG_LEVEL])
				c.DisableSync = false
				if !firstboot {
					ctrlCh <- struct{}{}
				}
			}
		},
		UpdateFunc: func(old, obj interface{}) {
			cm, ok := validateAviConfigMap(obj)
			oldcm, oldok := validateAviConfigMap(old)
			if ok && oldok {
				// if resourceversions and loglevel change, set new loglevel
				if oldcm.ResourceVersion != cm.ResourceVersion &&
					oldcm.Data[lib.LOG_LEVEL] != cm.Data[lib.LOG_LEVEL] {
					utils.AviLog.SetLevel(cm.Data[lib.LOG_LEVEL])
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if _, ok := validateAviConfigMap(obj); ok {
				utils.AviLog.Infof("avi k8s configmap deleted")
				c.DisableSync = true
				firstboot = false
				c.DeleteModels()
			}
		},
	}

	c.informers.ConfigMapInformer.Informer().AddEventHandler(configMapEventHandler)
	go c.informers.ConfigMapInformer.Informer().Run(stopCh)
}

func (c *AviController) InitController(informers K8sinformers, ctrlCh <-chan struct{}, stopCh <-chan struct{}) {
	// set up signals so we handle the first shutdown signal gracefully
	var worker *utils.FullSyncThread
	registeredInformers := []string{
		utils.ServiceInformer,
		utils.EndpointInformer,
		utils.IngressInformer,
		utils.SecretInformer,
		utils.NSInformer,
		utils.NodeInformer,
		utils.ConfigMapInformer,
	}
	if lib.GetNamespaceToSync() != "" {
		namespaceMap := make(map[string]interface{})
		namespaceMap[utils.INFORMERS_NAMESPACE] = lib.GetNamespaceToSync()
		c.informers = utils.NewInformers(utils.KubeClientIntf{ClientSet: informers.Cs}, registeredInformers, namespaceMap)
	} else {
		c.informers = utils.NewInformers(utils.KubeClientIntf{ClientSet: informers.Cs}, registeredInformers)
	}
	c.dynamicInformers = lib.NewDynamicInformers(informers.DynamicClient)

	c.Start(stopCh)
	/** Sequence:
	  1. Initialize the graph layer queue.
	  2. Do a full sync from main thread and publish all the models.
	  3. Initialize the ingestion layer queue for partial sync.
	  **/
	// start the go routines draining the queues in various layers
	var graphQueue *utils.WorkerQueue
	shardScheme := lib.GetShardScheme()
	// This is the first time initialization of the queue. For hostname based sharding, we don't want layer 2 to process the queue using multiple go routines.
	var retryQueueWorkers uint32
	retryQueueWorkers = 1
	slowRetryQParams := utils.WorkerQueue{NumWorkers: retryQueueWorkers, WorkqueueName: lib.SLOW_RETRY_LAYER, SlowSyncTime: lib.SLOW_SYNC_TIME}
	fastRetryQParams := utils.WorkerQueue{NumWorkers: retryQueueWorkers, WorkqueueName: lib.FAST_RETRY_LAYER}
	if shardScheme == lib.HOSTNAME_SHARD_SCHEME {
		var numWorkers uint32
		numWorkers = 1
		ingestionQueueParams := utils.WorkerQueue{NumWorkers: numWorkers, WorkqueueName: utils.ObjectIngestionLayer}
		numGraphWorkers := lib.GetshardSize()
		graphQueueParams := utils.WorkerQueue{NumWorkers: numGraphWorkers, WorkqueueName: utils.GraphLayer}
		graphQueue = utils.SharedWorkQueue(ingestionQueueParams, graphQueueParams, slowRetryQParams, fastRetryQParams).GetQueueByName(utils.GraphLayer)
	} else {
		// Namespace sharding.
		ingestionQueueParams := utils.WorkerQueue{NumWorkers: utils.NumWorkersIngestion, WorkqueueName: utils.ObjectIngestionLayer}
		graphQueueParams := utils.WorkerQueue{NumWorkers: utils.NumWorkersGraph, WorkqueueName: utils.GraphLayer}
		graphQueue = utils.SharedWorkQueue(ingestionQueueParams, graphQueueParams, slowRetryQParams, fastRetryQParams).GetQueueByName(utils.GraphLayer)
	}
	graphQueue.SyncFunc = SyncFromNodesLayer
	graphQueue.Run(stopCh)
	fullSyncInterval := os.Getenv(utils.FULL_SYNC_INTERVAL)
	interval, err := strconv.ParseInt(fullSyncInterval, 10, 64)
	if err != nil {
		utils.AviLog.Errorf("Cannot convert full sync interval value to integer, pls correct the value and restart AKC. Error: %s", err)
	} else {
		// First boot sync
		c.FullSyncK8s()
		if interval != 0 {
			worker = utils.NewFullSyncThread(time.Duration(interval) * time.Second)
			worker.SyncFunction = c.FullSync
			worker.QuickSyncFunction = c.FullSyncK8s
			go worker.Run()
		} else {
			utils.AviLog.Warnf("Full sync interval set to 0, will not run full sync")
		}
	}
	c.SetupEventHandlers(informers)
	ingestionQueue := utils.SharedWorkQueue().GetQueueByName(utils.ObjectIngestionLayer)
	ingestionQueue.SyncFunc = SyncFromIngestionLayer
	ingestionQueue.Run(stopCh)
	slowRetryQueue := utils.SharedWorkQueue().GetQueueByName(lib.SLOW_RETRY_LAYER)
	slowRetryQueue.SyncFunc = SyncFromSlowRetryLayer
	slowRetryQueue.Run(stopCh)
	fastRetryQueue := utils.SharedWorkQueue().GetQueueByName(lib.FAST_RETRY_LAYER)
	fastRetryQueue.SyncFunc = SyncFromFastRetryLayer
	fastRetryQueue.Run(stopCh)
	for {
		select {
		case <-ctrlCh:
			worker.QuickSync()
		case <-stopCh:
			break
		}
	}

	if worker != nil {
		worker.Shutdown()
	}
	ingestionQueue.StopWorkers(stopCh)
	graphQueue.StopWorkers(stopCh)
}

func (c *AviController) FullSync() {
	//func FullSync() {
	avi_rest_client_pool := avicache.SharedAVIClients()
	avi_obj_cache := avicache.SharedAviObjCache()
	// Randomly pickup a client.
	if len(avi_rest_client_pool.AviClient) > 0 {
		avi_obj_cache.AviCacheRefresh(avi_rest_client_pool.AviClient[0], utils.CloudName)
		allModelsMap := objects.SharedAviGraphLister().GetAll()
		var allModels []string
		for modelName, _ := range allModelsMap.(map[string]interface{}) {
			allModels = append(allModels, modelName)
		}
		for _, modelName := range allModels {
			utils.AviLog.Debugf("Reseting retry counter during full sync for model :%s", modelName)
			//reset retry counter in full sync
			found, avimodelIntf := objects.SharedAviGraphLister().Get(modelName)
			if found && avimodelIntf != nil {
				avimodel, ok := avimodelIntf.(*nodes.AviObjectGraph)
				if ok {
					avimodel.SetRetryCounter()
				}
			}
			// Not publishing the model anymore to layer since we don't want to support full sync for now.
			//nodes.PublishKeyToRestLayer(modelName, "fullsync", sharedQueue)
		}
	}
}

func (c *AviController) FullSyncK8s() {
	if c.DisableSync {
		utils.AviLog.Infof("Sync disabled, skipping full sync")
		return
	}
	// List all the kubernetes resources
	namespaces, err := utils.GetInformers().NSInformer.Lister().List(labels.Set(nil).AsSelector())
	if err != nil {
		utils.AviLog.Errorf("Unable to list the namespaces")
		return
	}
	for _, nsObj := range namespaces {
		svcObjs, err := utils.GetInformers().ServiceInformer.Lister().Services(nsObj.ObjectMeta.Name).List(labels.Set(nil).AsSelector())
		if err != nil {
			utils.AviLog.Errorf("Unable to retrieve the services during full sync: %s", err)
			continue
		}
		for _, svcObj := range svcObjs {
			isSvcLb := isServiceLBType(svcObj)
			var key string
			if isSvcLb {
				key = utils.L4LBService + "/" + utils.ObjKey(svcObj)
			} else {
				key = utils.Service + "/" + utils.ObjKey(svcObj)
			}
			nodes.DequeueIngestion(key, true)
		}

		ingObjs, err := utils.GetInformers().IngressInformer.Lister().ByNamespace(nsObj.ObjectMeta.Name).List(labels.Set(nil).AsSelector())
		if err != nil {
			utils.AviLog.Errorf("Unable to retrieve the ingresses during full sync: %s", err)
			continue
		}
		for _, ingObj := range ingObjs {
			key := utils.Ingress + "/" + utils.ObjKey(ingObj)
			nodes.DequeueIngestion(key, true)
		}

	}

	if os.Getenv(lib.DISABLE_STATIC_ROUTE_SYNC) == "true" {
		utils.AviLog.Infof("Static route sync disabled, skipping node informers")
	} else {
		nodeObjects, _ := utils.GetInformers().NodeInformer.Lister().List(labels.Set(nil).AsSelector())
		for _, node := range nodeObjects {
			key := utils.NodeObj + "/" + node.Name
			nodes.DequeueIngestion(key, true)
		}
	}

	cache := avicache.SharedAviObjCache()
	vsKeys := cache.VsCacheMeta.AviCacheGetAllParentVSKeys()
	utils.AviLog.Infof("Got the VS keys :%s", vsKeys)
	sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
	allModelsMap := objects.SharedAviGraphLister().GetAll()
	var allModels []string
	for modelName, _ := range allModelsMap.(map[string]interface{}) {
		allModels = append(allModels, modelName)
	}
	if len(vsKeys) != 0 {
		for _, vsCacheKey := range vsKeys {
			// Reverse map the model key from this.
			if lib.GetNamespaceToSync() != "" {
				shardVsPrefix := os.Getenv("SHARD_VS_PREFIX")
				if shardVsPrefix != "" {
					if strings.HasPrefix(vsCacheKey.Name, shardVsPrefix) {
						modelName := vsCacheKey.Namespace + "/" + vsCacheKey.Name
						if utils.HasElem(allModels, modelName) {
							allModels = utils.Remove(allModels, modelName)
						}
						utils.AviLog.Infof("Model published L7 VS during namespace based sync: %s", modelName)
						nodes.PublishKeyToRestLayer(modelName, "fullsync", sharedQueue)
					}
				}
				// For namespace based syncs, the L4 VSes would be named: vrfName + "--" + namespace
				if strings.HasPrefix(vsCacheKey.Name, lib.GetVrf()+"--"+lib.GetNamespaceToSync()) {
					modelName := vsCacheKey.Namespace + "/" + vsCacheKey.Name
					if utils.HasElem(allModels, modelName) {
						allModels = utils.Remove(allModels, modelName)
					}
					utils.AviLog.Infof("Model published L4 VS during namespace based sync: %s", modelName)
					nodes.PublishKeyToRestLayer(modelName, "fullsync", sharedQueue)
				}
			} else {
				modelName := vsCacheKey.Namespace + "/" + vsCacheKey.Name
				if utils.HasElem(allModels, modelName) {
					allModels = utils.Remove(allModels, modelName)
				}
				utils.AviLog.Infof("Model published in full sync %s", modelName)
				nodes.PublishKeyToRestLayer(modelName, "fullsync", sharedQueue)
			}
		}
	}
	// Now also publish the newly generated models (if any)
	// Publish all the models to REST layer.
	utils.AviLog.Infof("Newly generated models that do not exist in cache %s", utils.Stringify(allModels))
	if allModels != nil {
		for _, modelName := range allModels {
			nodes.PublishKeyToRestLayer(modelName, "fullsync", sharedQueue)
		}
	}
	return
}

// DeleteModels : Delete models and add the model name in the queue.
// The rest layer would pick up the model key and delete the objects in Avi
func (c *AviController) DeleteModels() {
	allModels := objects.SharedAviGraphLister().GetAll()
	sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
	for modelName, avimodelIntf := range allModels.(map[string]interface{}) {
		avimodel := avimodelIntf.(*nodes.AviObjectGraph)
		// for vrf, delete all static routes
		if avimodel.IsVrf {
			newAviModel := nodes.NewAviObjectGraph()
			newAviModel.IsVrf = true
			aviVrfNode := &nodes.AviVrfNode{
				Name: lib.GetVrf(),
			}
			newAviModel.AddModelNode(aviVrfNode)
			newAviModel.CalculateCheckSum()
			objects.SharedAviGraphLister().Save(modelName, newAviModel)
		} else {
			objects.SharedAviGraphLister().Save(modelName, nil)
		}
		bkt := utils.Bkt(modelName, sharedQueue.NumWorkers)
		sharedQueue.Workqueue[bkt].AddRateLimited(modelName)
	}
}

func SyncFromIngestionLayer(key string) error {
	// This method will do all necessary graph calculations on the Graph Layer
	// Let's route the key to the graph layer.
	// NOTE: There's no error propagation from the graph layer back to the workerqueue. We will evaluate
	// This condition in the future and visit as needed. But right now, there's no necessity for it.
	//sharedQueue := SharedWorkQueueWrappers().GetQueueByName(queue.GraphLayer)
	nodes.DequeueIngestion(key, false)
	return nil
}

func SyncFromSlowRetryLayer(key string) error {
	retry.DequeueSlowRetry(key)
	return nil
}

func SyncFromFastRetryLayer(key string) error {
	retry.DequeueFastRetry(key)
	return nil
}

func SyncFromNodesLayer(key string) error {
	cache := avicache.SharedAviObjCache()
	aviclient := avicache.SharedAVIClients()
	restlayer := rest.NewRestOperations(cache, aviclient)
	restlayer.DeQueueNodes(key)
	return nil
}