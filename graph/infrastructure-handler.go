package graph

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/SunSince90/ASTRID-kube/utils"

	informer "github.com/SunSince90/ASTRID-kube/informers"
	astrid_types "github.com/SunSince90/ASTRID-kube/types"
	log "github.com/sirupsen/logrus"
	apps_v1 "k8s.io/api/apps/v1"
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type Infrastructure interface {
}

type InfrastructureHandler struct {
	clientset           kubernetes.Interface
	name                string
	log                 *log.Entry
	labels              map[string]string
	deploymentsInformer informer.Informer
	servicesInformer    informer.Informer
	podInformer         informer.Informer
	depBarrier          chan struct{}
	servBarrier         chan struct{}
	resources           map[string]bool
	deployments         map[string]*count
	services            map[string]*core_v1.ServiceSpec
	lock                sync.Mutex
	infoBuilder         InfrastructureInfo
	initialized         bool
}

type count struct {
	needed  int32
	current int32
}

type serviceInfo struct {
	nodePort   int32
	targetPort int32
}

func new(clientset kubernetes.Interface, namespace *core_v1.Namespace) (Infrastructure, error) {
	//	the handler
	inf := &InfrastructureHandler{
		name:        namespace.Name,
		labels:      namespace.Labels,
		depBarrier:  make(chan struct{}),
		servBarrier: make(chan struct{}),
		clientset:   clientset,
		deployments: map[string]*count{},
		services:    map[string]*core_v1.ServiceSpec{},
		resources:   map[string]bool{},
		log:         log.New().WithFields(log.Fields{"GRAPH": namespace.Name}),
		initialized: false,
		infoBuilder: newBuilder(clientset, namespace.Name),
	}

	inf.log.Infoln("Starting graph handler for graph", namespace.Name)

	if len(namespace.Annotations) < 1 {
		inf.log.Errorln("Namespace has no annotations. Will stop here.")
		return nil, errors.New("Namespace has no annotations. Will stop here")
	}

	//	Get all deployments needed
	for name := range namespace.Annotations {
		if strings.HasPrefix(name, "astrid.io/") {
			actualName := strings.Split(name, "/")[1]
			inf.resources[actualName] = true
		}
	}

	//	First let's look at deployments
	deploymentsInformer := informer.New(astrid_types.Deployments, namespace.Name)
	deploymentsInformer.AddEventHandler(func(obj interface{}) {
		d := obj.(*apps_v1.Deployment)
		inf.handleNewDeployment(d)
	}, nil, nil)
	inf.deploymentsInformer = deploymentsInformer
	deploymentsInformer.Start()

	//	and then at services
	servInformer := informer.New(astrid_types.Services, namespace.Name)
	servInformer.AddEventHandler(func(obj interface{}) {
		s := obj.(*core_v1.Service)
		inf.handleNewService(s)
	}, nil, nil)
	inf.servicesInformer = servInformer
	servInformer.Start()

	go inf.listen()

	return inf, nil
}

func (handler *InfrastructureHandler) handleNewDeployment(deployment *apps_v1.Deployment) {
	handler.lock.Lock()
	defer handler.lock.Unlock()

	handler.log.Infoln("Detected deployment with name:", deployment.Name)

	//	Get replicas
	handler.deployments[deployment.Name] = &count{
		needed:  *deployment.Spec.Replicas,
		current: 0,
	}

	//	Do we have all deployments?
	if len(handler.deployments) != len(handler.resources) {
		return
	}
	for deployment := range handler.resources {
		if _, exists := handler.deployments[deployment]; !exists {
			return
		}
	}
	close(handler.depBarrier)
}

func (handler *InfrastructureHandler) handleNewService(service *core_v1.Service) {
	handler.lock.Lock()
	defer handler.lock.Unlock()

	handler.log.Infoln("Detected service with name:", service.Name)

	handler.services[service.Name] = &service.Spec
	handler.infoBuilder.PushService(service.Name, &service.Spec)

	//	Do we have all services?
	if len(handler.services) != len(handler.resources) {
		return
	}
	for deployment := range handler.resources {
		if _, exists := handler.services[deployment]; !exists {
			return
		}
	}
	close(handler.servBarrier)
}

func (handler *InfrastructureHandler) listen() {
	//	Wait for services discovery
	<-handler.servBarrier
	handler.log.Infoln("Found all services needed for this graph")

	//	Wait for deployments discovery
	<-handler.depBarrier
	handler.log.Infoln("Found all deployments needed for this graph")

	handler.log.Infoln("Started listening for pod life cycle events...")
	//	Start listening for pods
	podInformer := informer.New(astrid_types.Pods, handler.name)
	podInformer.AddEventHandler(func(obj interface{}) {
		p := obj.(*core_v1.Pod)
		handler.handlePod(p)
	}, func(old, obj interface{}) {
		p := obj.(*core_v1.Pod)
		handler.handlePod(p)
	}, func(obj interface{}) {
		p := obj.(*core_v1.Pod)
		handler.log.Infoln("Detected dead pod:", p.Name)
		handler.infoBuilder.PopInstance(p.Name)
	})
	handler.podInformer = podInformer
	handler.podInformer.Start()
}

func (handler *InfrastructureHandler) handlePod(pod *core_v1.Pod) {
	if pod.Status.Phase != core_v1.PodRunning {
		return
	}

	//	This is improbable, but just in case
	if len(pod.Labels) < 1 {
		return
	}

	if pod.ObjectMeta.DeletionTimestamp != nil {
		return
	}

	//	Doing it here so we can speed up some parts
	shouldStop := func() (*count, bool) {
		handler.lock.Lock()
		defer handler.lock.Unlock()

		depName, exists := pod.Labels["astrid.io/deployment"]
		if !exists {
			handler.log.Errorln(pod.Name, "does not have a deployment label")
			return nil, true
		}

		dep, exists := handler.deployments[depName]
		if !exists {
			handler.log.Errorln(depName, "does not exist")
			return nil, true
		}

		return dep, false
	}

	dep, stop := shouldStop()
	if stop {
		return
	}

	handler.log.Infoln("Detected running pod:", pod.Name)
	time.AfterFunc(time.Second*10, func() {
		handler.setupFirewall(pod, dep)
	})
}

func (handler *InfrastructureHandler) setupFirewall(pod *core_v1.Pod, dep *count) {
	//	shorthands
	ip := pod.Status.PodIP
	name := pod.Name
	service := pod.Labels["astrid.io/service"]

	if !utils.CreateFirewall(ip) {
		return
	}
	handler.log.Infoln("Created firewall for pod:", name)

	//	TODO: uncomment this when firewall is fixed
	/*if !utils.AttachFirewall(ip) {
		return
	}
	handler.log.Infoln("Attached firewall to pod:", name)*/

	//	TODO: look into name as uid
	handler.infoBuilder.PushInstance(service, ip, name)

	handler.lock.Lock()
	defer handler.lock.Unlock()

	if handler.initialized {
		return
	}
	dep.current++
	if dep.current == dep.needed {
		handler.canBuildInfo()
	}

}

func (handler *InfrastructureHandler) canBuildInfo() {
	//	It is better to have it like this rather than having a counter, as this is more robust for unstable pods
	for _, dep := range handler.deployments {
		if dep.current != dep.needed {
			return
		}
	}

	handler.initialized = true
	handler.log.Infoln("The graph is fully running. Building Infrastructure Info...")
	handler.infoBuilder.ToggleSending()
}
