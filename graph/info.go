package graph

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/SunSince90/ASTRID-kube/utils"

	"github.com/SunSince90/ASTRID-kube/settings"

	log "github.com/sirupsen/logrus"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	types "github.com/SunSince90/ASTRID-kube/types"
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type InfrastructureInfo interface {
	PushService(string, *core_v1.ServiceSpec, []string)
	PushInstance(string, string, string)
	PopInstance(string)
	EnableSending()
	//Build(types.EncodingType)
}

type InfrastructureInfoBuilder struct {
	lock              sync.Mutex
	info              types.InfrastructureInfo
	deployedServices  map[string]*serviceOffset
	deployedInstances map[string]*instanceOffset
	clientset         kubernetes.Interface
	sendingMode       string
}

type serviceOffset struct {
	securityComponents []string
	position           int
}

type instanceOffset struct {
	value    string
	position int
	owner    string
}

func newBuilder(clientset kubernetes.Interface, name string) InfrastructureInfo {

	info := types.InfrastructureInfo{
		Kind: types.KIND,
		Metadata: types.InfrastructureInfoMetadata{
			Name:       name,
			LastUpdate: time.Now().UTC(),
		},
	}

	return &InfrastructureInfoBuilder{
		info:              info,
		clientset:         clientset,
		deployedServices:  map[string]*serviceOffset{},
		deployedInstances: map[string]*instanceOffset{},
		sendingMode:       "",
	}
}

func (i *InfrastructureInfoBuilder) PushService(name string, spec *core_v1.ServiceSpec, securityComponents []string) {
	i.lock.Lock()
	defer i.lock.Unlock()

	if _, exists := i.deployedServices[name]; exists {
		return
	}

	i.deployedServices[name] = &serviceOffset{
		position:           len(i.info.Spec.Services),
		securityComponents: securityComponents,
	}
	service := types.InfrastructureInfoService{
		Name: name,
	}

	//	Put security components
	for _, sc := range securityComponents {
		service.SecurityComponents = append(service.SecurityComponents, types.InfrastructureInfoSecurityComponent{
			Name: sc,
		})
	}

	for _, ports := range spec.Ports {
		if ports.Name == name+"-ambassador-port" {
			service.AmbassadorPort = types.InfrastructureInfoServicePort{
				Port:     9000,
				Exposed:  ports.NodePort,
				Protocol: types.TCP,
			}
		} else {
			var protocol types.InfrastructureInfoProtocol
			switch ports.Protocol {
			case core_v1.ProtocolTCP:
				protocol = types.TCP
			case core_v1.ProtocolUDP:
				protocol = types.UDP
			}

			service.Ports = append(service.Ports, types.InfrastructureInfoServicePort{
				Port:     ports.TargetPort.IntVal,
				Exposed:  ports.NodePort,
				Protocol: protocol,
			})
		}
	}

	i.info.Spec.Services = append(i.info.Spec.Services, service)
}

func (i *InfrastructureInfoBuilder) PushInstance(service, ip, uid string) {
	i.lock.Lock()
	defer i.lock.Unlock()

	s, exists := i.deployedServices[service]
	if !exists {
		return
	}
	serviceOffset := s.position

	existingIP, exists := i.deployedInstances[uid]
	if exists {
		if existingIP.value == ip {
			return
		}
		existingIP.value = ip
	} else {
		i.deployedInstances[uid] = &instanceOffset{
			position: len(i.info.Spec.Services[serviceOffset].Instances),
			value:    ip,
			owner:    service,
		}
	}

	i.info.Spec.Services[serviceOffset].Instances = append(i.info.Spec.Services[serviceOffset].Instances, types.InfrastructureInfoServiceInstance{
		IP:  ip,
		UID: uid,
	})

	i.send()
}

func (i *InfrastructureInfoBuilder) PopInstance(uid string) {
	i.lock.Lock()
	defer i.lock.Unlock()

	instance, exists := i.deployedInstances[uid]
	if !exists {
		return
	}

	s, exists := i.deployedServices[instance.owner]
	if !exists {
		return
	}
	serviceOffset := s.position

	//	Only one?
	if len(i.info.Spec.Services[serviceOffset].Instances) == 1 {
		i.info.Spec.Services[serviceOffset].Instances = []types.InfrastructureInfoServiceInstance{}
	} else {
		//	swap
		t := instance.position
		i.info.Spec.Services[serviceOffset].Instances = append(i.info.Spec.Services[serviceOffset].Instances[:t], i.info.Spec.Services[serviceOffset].Instances[t+1:]...)
	}
	i.send()
}

func (i *InfrastructureInfoBuilder) EnableSending() {
	i.sendingMode = "infrastructure-info"

	//	Send immediately
	i.send()
}

/*func (i *InfrastructureInfoBuilder) Build(to types.EncodingType) {
	i.lock.Lock()
	defer i.lock.Unlock()

	nodes, err := i.clientset.CoreV1().Nodes().List(meta_v1.ListOptions{})
	if err != nil {
		log.Errorln("Cannot get nodes:", err)
		return
	}

	if len(i.info.Spec.Nodes) < 1 {
		for _, node := range nodes.Items {
			i.info.Spec.Nodes = append(i.info.Spec.Nodes, types.InfrastructureInfoNode{
				//	TODO: check this out
				IP: node.Status.Addresses[0].Address,
			})
		}
	}

	yaml := func() {
		data, err := yaml.Marshal(&i.info)
		if err != nil {
			log.Errorln("Cannot marshal to yaml:", err)
			return
		}
		log.Printf("--- t dump:\n%s\n\n", string(data))
	}

	xml := func() {
		data, err := xml.MarshalIndent(&i.info, "", "   ")
		if err != nil {
			log.Errorln("Cannot marshal to xml:", err)
			return
		}
		log.Printf("--- t dump:\n%s\n\n", string(data))
	}

	json := func() {
		data, err := json.MarshalIndent(&i.info, "", "   ")
		if err != nil {
			log.Errorln("Cannot marshal to json:", err)
			return
		}
		log.Printf("--- t dump:\n%s\n\n", string(data))
	}

	switch to {
	case types.XML:
		xml()
	case types.YAML:
		yaml()
	case types.JSON:
		json()
	}
}*/

func (i *InfrastructureInfoBuilder) generate() ([]byte, string, error) {

	infrastructureInfo := func() ([]byte, string, error) {
		i.info.Metadata.LastUpdate = time.Now().UTC()
		nodes, err := i.clientset.CoreV1().Nodes().List(meta_v1.ListOptions{})
		if err != nil {
			log.Errorln("Cannot get nodes:", err)
			return nil, "", nil
		}

		if len(i.info.Spec.Nodes) < 1 {
			for _, node := range nodes.Items {
				i.info.Spec.Nodes = append(i.info.Spec.Nodes, types.InfrastructureInfoNode{
					//	TODO: check this out
					IP: node.Status.Addresses[0].Address,
				})
			}
		}

		data, contentType, err := utils.Marshal(settings.Settings.Formats.InfrastructureInfo, i.info)
		log.Printf("# --- Infrastructure Info to send: --- #:\n%s\n\n# --- /Infrastructure Info to send --- #", string(data))
		return data, contentType, nil
	}

	switch i.sendingMode {
	case "infrastructure-info":
		return infrastructureInfo()
	}

	return nil, "", nil
}

func (i *InfrastructureInfoBuilder) send() {
	if len(i.sendingMode) < 1 {
		return
	}

	data, contentType, err := i.generate()
	if err != nil {
		return
	}

	i.sendRequest(data, contentType)
}

func (i *InfrastructureInfoBuilder) sendRequest(data []byte, contentType string) {
	//	TODO: change endpoint based on event or info
	endPoint := settings.Settings.EndPoints.Verekube
	req, err := http.NewRequest("POST", endPoint, bytes.NewBuffer(data))
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Errorln("Error while trying to send request:", err)
		return
	}
	defer resp.Body.Close()

	fmt.Println("Sent infrastructure info and received", resp.Status)
}
