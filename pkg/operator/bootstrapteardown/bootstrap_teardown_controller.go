package bootstrapteardown

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"

	"github.com/davecgh/go-spew/spew"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey              = "key"
	configMapName             = "config"
	configMapKey              = "config.yaml"
	conditionBootstrapRemoved = "BootstrapRemoved"
)

type BootstrapTeardownController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
	kubeClient     kubernetes.Interface

	kubeAPIServerLister operatorv1listers.KubeAPIServerLister
	configMapLister     corev1listers.ConfigMapLister
	endpointLister      corev1listers.EndpointsLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewBootstrapTeardownController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,

	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	operatorInformers operatorv1informers.SharedInformerFactory,
	etcdClient etcdcli.EtcdClient,

	eventRecorder events.Recorder,
) *BootstrapTeardownController {
	openshiftKubeAPIServerNamespacedInformers := kubeInformersForNamespaces.InformersFor("openshift-kube-apiserver")
	c := &BootstrapTeardownController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
		kubeClient:     kubeClient,

		kubeAPIServerLister: operatorInformers.Operator().V1().KubeAPIServers().Lister(),
		configMapLister:     openshiftKubeAPIServerNamespacedInformers.Core().V1().ConfigMaps().Lister(),
		endpointLister:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			operatorInformers.Operator().V1().Etcds().Informer().HasSynced,
			operatorInformers.Operator().V1().KubeAPIServers().Informer().HasSynced,
			openshiftKubeAPIServerNamespacedInformers.Core().V1().ConfigMaps().Informer().HasSynced,
			openshiftKubeAPIServerNamespacedInformers.Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "BootstrapTeardownController"),
		eventRecorder: eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"),
	}

	operatorInformers.Operator().V1().KubeAPIServers().Informer().AddEventHandler(c.eventHandler())
	openshiftKubeAPIServerNamespacedInformers.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	openshiftKubeAPIServerNamespacedInformers.Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())
	operatorClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *BootstrapTeardownController) sync() error {
	err := c.removeBootstrap()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapTeardownDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BootstrapTeardownDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap() error {
	// checks the actual etcd cluster membership API if etcd-bootstrap exists
	etcdMemberExists, err := c.isEtcdMember("etcd-bootstrap")
	if err != nil {
		return err
	}
	if !etcdMemberExists {
		// set bootstrap removed condition
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    conditionBootstrapRemoved,
			Status:  operatorv1.ConditionTrue,
			Reason:  "BootstrapNodeRemoved",
			Message: "Etcd operator has scaled",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		// return because no work left to do
		return nil

	} else {
		_, _, _ = v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    conditionBootstrapRemoved,
			Status:  operatorv1.ConditionFalse,
			Reason:  "BootstrapNodeNotRemoved",
			Message: fmt.Sprintf("Bootstrap node is not removed yet: etcdMemberExists %t", etcdMemberExists),
		}))
		// fall through because it might be time to remove it
	}

	hasMoreThanTwoEtcdMembers, err := c.hasMoreThanTwoEtcdMembers()
	if err != nil {
		return err
	}
	if !hasMoreThanTwoEtcdMembers {
		c.eventRecorder.Event("NotEnoughEtcdMembers", "Still waiting for three etcd members")
		return nil
	}

	kubeAPIServer, err := c.kubeAPIServerLister.Get("cluster")
	if err != nil {
		return err
	}
	kasReady, err := c.isKASConfigValid(kubeAPIServer, c.configMapLister)
	if err != nil {
		return err
	}
	if !kasReady {
		c.eventRecorder.Event("KASConfigIsNotValid", "Still waiting for the kube-apiserver to be ready")
		return nil
	}

	// check to see if bootstrapping is complete
	bootstrapFinished := false
	bootstrapFinishedConfigMap, err := c.kubeClient.CoreV1().ConfigMaps("kube-system").Get("bootstrap", metav1.GetOptions{})
	if err != nil {
		klog.V(2).Infof("non-fatal error getting configmap: %v", err)
	} else {
		if bootstrapFinishedConfigMap.Data["status"] == "complete" {
			bootstrapFinished = true
		}
	}
	if !bootstrapFinished {
		// check to see if we have an event yet.
		_, err := c.kubeClient.CoreV1().Events("kube-system").Get("bootstrap-finished", metav1.GetOptions{})
		if err != nil {
			klog.V(2).Infof("non-fatal error getting bootstrap-finished event: %v", err)
		} else {
			bootstrapFinished = true
		}
	}

	if !bootstrapFinished {
		c.eventRecorder.Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	}

	c.eventRecorder.Event("BootstrapTeardownController", "safe to remove bootstrap")
	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	time.Sleep(10 * time.Second)
	if err := c.etcdClient.MemberRemove("etcd-bootstrap"); err != nil {
		return err
	}

	return nil
}

func (c *BootstrapTeardownController) isEtcdMember(name string) (bool, error) {
	members, err := c.etcdClient.MemberList()
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (c *BootstrapTeardownController) hasMoreThanTwoEtcdMembers() (bool, error) {
	members, err := c.etcdClient.MemberList()
	if err != nil {
		return false, err
	}
	klog.Infof("members %v", spew.Sdump(members))

	if len(members) <= 3 {
		return false, nil
	}

	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
	if err != nil {
		return false, nil
	}
	klog.Infof("unhealthy members %v", spew.Sdump(members))
	if len(unhealthyMembers) > 0 {
		return false, nil
	}
	return true, nil
}

func (c *BootstrapTeardownController) isKASConfigValid(kasOperator *operatorv1.KubeAPIServer, configMapLister corev1listers.ConfigMapLister) (bool, error) {
	revisionMap := map[int32]struct{}{}
	uniqueRevisions := []int32{}

	for _, nodeStatus := range kasOperator.Status.NodeStatuses {
		revision := nodeStatus.CurrentRevision
		if _, ok := revisionMap[revision]; !ok {
			revisionMap[revision] = struct{}{}
			uniqueRevisions = append(uniqueRevisions, revision)
		}
	}

	// For each revision, check that the configmap for that revision contains the
	// appropriate storageConfig
	for _, revision := range uniqueRevisions {
		configMapNameWithRevision := fmt.Sprintf("%s-%d", configMapName, revision)
		configMap, err := configMapLister.ConfigMaps("openshift-kube-apiserver").Get(configMapNameWithRevision)
		if err != nil {
			return false, err
		}
		if c.configMapHasRequiredValues(configMap) {
			// if any 1 kube-apiserver pod has more than 1
			klog.V(4).Info("kube-apiserver has required values")
			return true, nil
		}
	}
	return false, nil
}

type ConfigData struct {
	StorageConfig struct {
		Urls []string
	}
}

func (c *BootstrapTeardownController) configMapHasRequiredValues(configMap *corev1.ConfigMap) bool {
	config, ok := configMap.Data[configMapKey]
	if !ok {
		c.eventRecorder.Eventf("KASconfigmapDoesNotHaveRequiredValues", "configMapHasRequiredValues: config.yaml key missing for configmap %s/%s", configMap.Namespace, configMap.Name)
		return false
	}
	var configData ConfigData
	err := json.Unmarshal([]byte(config), &configData)
	if err != nil {
		c.eventRecorder.Eventf("KASconfigmapDoesNotHaveRequiredValues", "error unmarshalling configmap %s/%s data: %#v", configMap.Namespace, configMap.Name, err)
		return false
	}
	if len(configData.StorageConfig.Urls) == 0 {
		c.eventRecorder.Eventf("KASconfigmapDoesNotHaveRequiredValues", "configMapHasRequiredValues: length of storageUrls is 0 for configmap %s/%s", configMap.Namespace, configMap.Name)
		return false
	}
	if len(configData.StorageConfig.Urls) == 1 &&
		!strings.Contains(configData.StorageConfig.Urls[0], "etcd") {
		c.eventRecorder.Eventf("KASconfigmapDoesNotHaveRequiredValues", "configMapHasRequiredValues: config %s/%s has a single IP: %#v", configMap.Namespace, configMap.Name, strings.Join(configData.StorageConfig.Urls, ", "))
		return false
	}
	return true
}

func (c *BootstrapTeardownController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting BootstrapTeardownController")
	defer klog.Infof("Shutting down BootstrapTeardownController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}
	klog.V(2).Infof("caches synced BootstrapTeardownController")

	go wait.Until(c.runWorker, time.Second, stopCh)

	// add time based trigger
	go wait.PollImmediateUntil(time.Minute, func() (bool, error) {
		c.queue.Add(workQueueKey)
		return false, nil
	}, stopCh)

	<-stopCh
}

func (c *BootstrapTeardownController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *BootstrapTeardownController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *BootstrapTeardownController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
