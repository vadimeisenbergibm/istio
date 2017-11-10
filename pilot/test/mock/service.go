// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mock

import (
	"fmt"
	"net"

	proxyconfig "istio.io/api/proxy/v1/config"
	"istio.io/istio/pilot/model"
	"istio.io/istio/pilot/proxy"
)

// Mock values
var (
	HelloService = MakeService("hello.default.svc.cluster.local", "10.1.0.0")
	WorldService = MakeService("world.default.svc.cluster.local", "10.2.0.0")
	PortHTTP     = &model.Port{
		Name:                 "http",
		Port:                 80, // target port 80
		Protocol:             model.ProtocolHTTP,
		AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
	}
	ExtHTTPService = MakeExternalHTTPService("httpbin.default.svc.cluster.local",
		"httpbin.org", "")
	ExtHTTPSService = MakeExternalHTTPSService("httpsbin.default.svc.cluster.local",
		"httpbin.org", "")
	Discovery = &ServiceDiscovery{
		services: map[string]*model.Service{
			HelloService.Hostname:   HelloService,
			WorldService.Hostname:   WorldService,
			ExtHTTPService.Hostname: ExtHTTPService,
			// TODO external https is not currently supported - this service
			// should NOT be in any of the .golden json files
			ExtHTTPSService.Hostname: ExtHTTPSService,
		},
		versions: 2,
	}
	HelloInstanceV0 = MakeIP(HelloService, 0)
	HelloInstanceV1 = MakeIP(HelloService, 1)
	HelloProxyV0    = proxy.Node{
		Type:      proxy.Sidecar,
		IPAddress: HelloInstanceV0,
		ID:        "v0.default",
		Domain:    "default.svc.cluster.local",
	}
	HelloProxyV1 = proxy.Node{
		Type:      proxy.Sidecar,
		IPAddress: HelloInstanceV1,
		ID:        "v1.default",
		Domain:    "default.svc.cluster.local",
	}
	Ingress = proxy.Node{
		Type:      proxy.Ingress,
		IPAddress: "10.3.3.3",
		ID:        "ingress.default",
		Domain:    "default.svc.cluster.local",
	}
	Router = proxy.Node{
		Type:      proxy.Router,
		IPAddress: "10.3.3.5",
		ID:        "router.default",
		Domain:    "default.svc.cluster.local",
	}
)

// NewDiscovery builds a mock ServiceDiscovery
func NewDiscovery(services map[string]*model.Service, versions int) *ServiceDiscovery {
	return &ServiceDiscovery{
		services: services,
		versions: versions,
	}
}

// MakeService creates a mock service
func MakeService(hostname, address string) *model.Service {
	return &model.Service{
		Hostname: hostname,
		Address:  address,
		Ports: []*model.Port{
			PortHTTP,
			{
				Name:                 "http-status",
				Port:                 81, // target port 1081
				Protocol:             model.ProtocolHTTP,
				AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
			}, {
				Name:                 "custom",
				Port:                 90, // target port 1090
				Protocol:             model.ProtocolTCP,
				AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
			}, {
				Name:                 "mongo",
				Port:                 100, // target port 1100
				Protocol:             model.ProtocolMongo,
				AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
			},
			{
				Name:                 "redis",
				Port:                 110, // target port 1110
				Protocol:             model.ProtocolRedis,
				AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
			}},
	}
}

// MakeExternalHTTPService creates mock external service
func MakeExternalHTTPService(hostname, external string, address string) *model.Service {
	return &model.Service{
		Hostname:     hostname,
		Address:      address,
		ExternalName: external,
		Ports: []*model.Port{{
			Name:                 "http",
			Port:                 80,
			Protocol:             model.ProtocolHTTP,
			AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
		}},
	}
}

// MakeExternalHTTPSService creates mock external service
func MakeExternalHTTPSService(hostname, external string, address string) *model.Service {
	return &model.Service{
		Hostname:     hostname,
		Address:      address,
		ExternalName: external,
		Ports: []*model.Port{{
			Name:                 "https",
			Port:                 443,
			Protocol:             model.ProtocolHTTPS,
			AuthenticationPolicy: proxyconfig.AuthenticationPolicy_INHERIT,
		}},
	}
}

// MakeInstance creates a mock instance, version enumerates endpoints
func MakeInstance(service *model.Service, port *model.Port, version int) *model.ServiceInstance {
	if service.External() {
		return nil
	}

	// we make port 80 same as endpoint port, otherwise, it's distinct
	target := port.Port
	if target != 80 {
		target = target + 1000
	}

	return &model.ServiceInstance{
		Endpoint: model.NetworkEndpoint{
			Address:     MakeIP(service, version),
			Port:        target,
			ServicePort: port,
		},
		Service: service,
		Labels:  map[string]string{"version": fmt.Sprintf("v%d", version)},
	}
}

// MakeIP creates a fake IP address for a service and instance version
func MakeIP(service *model.Service, version int) string {
	// external services have no instances
	if service.External() {
		return ""
	}
	ip := net.ParseIP(service.Address).To4()
	ip[2] = byte(1)
	ip[3] = byte(version)
	return ip.String()
}

// ServiceDiscovery is a mock discovery interface
type ServiceDiscovery struct {
	services           map[string]*model.Service
	versions           int
	ServicesError      error
	GetServiceError    error
	InstancesError     error
	HostInstancesError error
}

// ClearErrors clear errors used for mocking failures during model.ServiceDiscovery interface methods
func (sd *ServiceDiscovery) ClearErrors() {
	sd.ServicesError = nil
	sd.GetServiceError = nil
	sd.InstancesError = nil
	sd.HostInstancesError = nil
}

// Services implements discovery interface
func (sd *ServiceDiscovery) Services() ([]*model.Service, error) {
	if sd.ServicesError != nil {
		return nil, sd.ServicesError
	}
	out := make([]*model.Service, 0, len(sd.services))
	for _, service := range sd.services {
		out = append(out, service)
	}
	return out, sd.ServicesError
}

// GetService implements discovery interface
func (sd *ServiceDiscovery) GetService(hostname string) (*model.Service, error) {
	if sd.GetServiceError != nil {
		return nil, sd.GetServiceError
	}
	val := sd.services[hostname]
	return val, sd.GetServiceError
}

// Instances implements discovery interface
func (sd *ServiceDiscovery) Instances(hostname string, ports []string,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	if sd.InstancesError != nil {
		return nil, sd.InstancesError
	}
	service, ok := sd.services[hostname]
	if !ok {
		return nil, sd.InstancesError
	}
	out := make([]*model.ServiceInstance, 0)
	if service.External() {
		return out, sd.InstancesError
	}
	for _, name := range ports {
		if port, ok := service.Ports.Get(name); ok {
			for v := 0; v < sd.versions; v++ {
				if labels.HasSubsetOf(map[string]string{"version": fmt.Sprintf("v%d", v)}) {
					out = append(out, MakeInstance(service, port, v))
				}
			}
		}
	}
	return out, sd.InstancesError
}

// HostInstances implements discovery interface
func (sd *ServiceDiscovery) HostInstances(addrs map[string]bool) ([]*model.ServiceInstance, error) {
	if sd.HostInstancesError != nil {
		return nil, sd.HostInstancesError
	}
	out := make([]*model.ServiceInstance, 0)
	for _, service := range sd.services {
		if !service.External() {
			for v := 0; v < sd.versions; v++ {
				if addrs[MakeIP(service, v)] {
					for _, port := range service.Ports {
						out = append(out, MakeInstance(service, port, v))
					}
				}
			}
		}
	}
	return out, sd.HostInstancesError
}

// ManagementPorts implements discovery interface
func (sd *ServiceDiscovery) ManagementPorts(addr string) model.PortList {
	return model.PortList{{
		Name:     "http",
		Port:     3333,
		Protocol: model.ProtocolHTTP,
	}, {
		Name:     "custom",
		Port:     9999,
		Protocol: model.ProtocolTCP,
	}}
}

// GetIstioServiceAccounts gets the Istio service accounts for a service hostname.
func (sd *ServiceDiscovery) GetIstioServiceAccounts(hostname string, ports []string) []string {
	if hostname == "world.default.svc.cluster.local" {
		return []string{
			"spiffe://cluster.local/ns/default/sa/serviceaccount1",
			"spiffe://cluster.local/ns/default/sa/serviceaccount2",
		}
	}
	return make([]string, 0)
}
