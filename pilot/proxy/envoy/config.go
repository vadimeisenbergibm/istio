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

package envoy

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"

	proxyconfig "istio.io/api/proxy/v1/config"
	"istio.io/istio/pilot/model"
	"istio.io/istio/pilot/proxy"
)

// Config generation main functions.
// The general flow of the generation process consists of the following steps:
// - routes are created for each destination, with referenced clusters stored as a special field
// - routes are organized into listeners for inbound and outbound traffic
// - clusters are aggregated and normalized across routes
// - extra policies and filters are added by additional passes over abstract config structures
// - configuration elements are de-duplicated and ordered in a canonical way

// WriteFile saves config to a file
func (conf *Config) WriteFile(fname string) error {
	if glog.V(2) {
		glog.Infof("writing configuration to %s", fname)
		if err := conf.Write(os.Stderr); err != nil {
			glog.Error(err)
		}
	}

	file, err := os.Create(fname)
	if err != nil {
		return err
	}

	if err := conf.Write(file); err != nil {
		err = multierror.Append(err, file.Close())
		return err
	}

	return file.Close()
}

func (conf *Config) Write(w io.Writer) error {
	out, err := json.MarshalIndent(&conf, "", "  ")
	if err != nil {
		return err
	}

	_, err = w.Write(out)
	return err
}

// buildConfig creates a proxy config with discovery services and admin port
// it creates config for Ingress, Egress and Sidecar proxies
func buildConfig(config proxyconfig.ProxyConfig, pilotSAN []string) *Config {
	listeners := Listeners{}

	clusterRDS := buildCluster(config.DiscoveryAddress, RDSName, config.ConnectTimeout)
	clusterLDS := buildCluster(config.DiscoveryAddress, LDSName, config.ConnectTimeout)
	clusters := Clusters{clusterRDS, clusterLDS}

	out := &Config{
		Listeners: listeners,
		LDS: &LDSCluster{
			Cluster:        LDSName,
			RefreshDelayMs: protoDurationToMS(config.DiscoveryRefreshDelay),
		},
		Admin: Admin{
			AccessLogPath: DefaultAccessLog,
			Address:       fmt.Sprintf("tcp://%s:%d", LocalhostAddress, config.ProxyAdminPort),
		},
		ClusterManager: ClusterManager{
			Clusters: clusters,
			SDS: &DiscoveryCluster{
				Cluster:        buildCluster(config.DiscoveryAddress, SDSName, config.ConnectTimeout),
				RefreshDelayMs: protoDurationToMS(config.DiscoveryRefreshDelay),
			},
			CDS: &DiscoveryCluster{
				Cluster:        buildCluster(config.DiscoveryAddress, CDSName, config.ConnectTimeout),
				RefreshDelayMs: protoDurationToMS(config.DiscoveryRefreshDelay),
			},
		},
		StatsdUDPIPAddress: config.StatsdUdpAddress,
	}

	// apply auth policies
	switch config.ControlPlaneAuthPolicy {
	case proxyconfig.AuthenticationPolicy_NONE:
		// do nothing
	case proxyconfig.AuthenticationPolicy_MUTUAL_TLS:
		sslContext := buildClusterSSLContext(proxy.AuthCertsPath, pilotSAN)
		clusterRDS.SSLContext = sslContext
		clusterLDS.SSLContext = sslContext
		out.ClusterManager.SDS.Cluster.SSLContext = sslContext
		out.ClusterManager.CDS.Cluster.SSLContext = sslContext
	default:
		panic(fmt.Sprintf("ControlPlaneAuthPolicy cannot be %v\n", config.ControlPlaneAuthPolicy))
	}

	if config.ZipkinAddress != "" {
		out.ClusterManager.Clusters = append(out.ClusterManager.Clusters,
			buildCluster(config.ZipkinAddress, ZipkinCollectorCluster, config.ConnectTimeout))
		out.Tracing = buildZipkinTracing()
	}

	return out
}

// buildListeners produces a list of listeners and referenced clusters for all proxies
func buildListeners(env proxy.Environment, node proxy.Node) (Listeners, error) {
	switch node.Type {
	case proxy.Sidecar, proxy.Router:
		instances, err := env.HostInstances(map[string]bool{node.IPAddress: true})
		if err != nil {
			return nil, err
		}
		services, err := env.Services()
		if err != nil {
			return nil, err
		}
		listeners, _ := buildSidecarListenersClusters(env.Mesh, instances,
			services, env.ManagementPorts(node.IPAddress), node, env.IstioConfigStore)
		return listeners, nil
	case proxy.Ingress:
		instances, err := env.HostInstances(map[string]bool{node.IPAddress: true})
		if err != nil {
			return Listeners{}, err
		}
		return buildIngressListeners(env.Mesh, instances, env.ServiceDiscovery, env.IstioConfigStore, node), nil
	}
	return nil, nil
}

func buildClusters(env proxy.Environment, node proxy.Node) (Clusters, error) {
	var clusters Clusters
	var instances []*model.ServiceInstance
	var err error
	switch node.Type {
	case proxy.Sidecar, proxy.Router:
		instances, err = env.HostInstances(map[string]bool{node.IPAddress: true})
		if err != nil {
			return clusters, err
		}
		services, err := env.Services()
		if err != nil {
			return clusters, err
		}
		_, clusters = buildSidecarListenersClusters(env.Mesh, instances,
			services, env.ManagementPorts(node.IPAddress), node, env.IstioConfigStore)
	case proxy.Ingress:
		instances, err = env.HostInstances(map[string]bool{node.IPAddress: true})
		httpRouteConfigs, _ := buildIngressRoutes(env.Mesh, instances, env.ServiceDiscovery, env.IstioConfigStore)
		clusters = httpRouteConfigs.clusters().normalize()
	}

	if err != nil {
		return clusters, err
	}

	// apply custom policies for outbound clusters
	for _, cluster := range clusters {
		applyClusterPolicy(cluster, instances, env.IstioConfigStore, env.Mesh, env.ServiceAccounts)
	}

	// append Mixer service definition if necessary
	if env.Mesh.MixerAddress != "" {
		clusters = append(clusters, buildMixerCluster(env.Mesh, node, env.MixerSAN))
	}

	return clusters, nil
}

// buildSidecarListenersClusters produces a list of listeners and referenced clusters for sidecar proxies
// TODO: this implementation is inefficient as it is recomputing all the routes for all proxies
// There is a lot of potential to cache and reuse cluster definitions across proxies and also
// skip computing the actual HTTP routes
func buildSidecarListenersClusters(
	mesh *proxyconfig.MeshConfig,
	instances []*model.ServiceInstance,
	services []*model.Service,
	managementPorts model.PortList,
	node proxy.Node,
	config model.IstioConfigStore) (Listeners, Clusters) {

	// ensure services are ordered to simplify generation logic
	sort.Slice(services, func(i, j int) bool { return services[i].Hostname < services[j].Hostname })

	listeners := make(Listeners, 0)
	clusters := make(Clusters, 0)

	if node.Type == proxy.Router {
		outbound, outClusters := buildOutboundListeners(mesh, node, instances, services, config)
		listeners = append(listeners, outbound...)
		clusters = append(clusters, outClusters...)
	} else if mesh.ProxyListenPort > 0 {
		inbound, inClusters := buildInboundListeners(mesh, node, instances, config)
		outbound, outClusters := buildOutboundListeners(mesh, node, instances, services, config)
		mgmtListeners, mgmtClusters := buildMgmtPortListeners(mesh, managementPorts, node.IPAddress)

		listeners = append(listeners, inbound...)
		listeners = append(listeners, outbound...)
		clusters = append(clusters, inClusters...)
		clusters = append(clusters, outClusters...)

		// If management listener port and service port are same, bad things happen
		// when running in kubernetes, as the probes stop responding. So, append
		// non overlapping listeners only.
		for i := range mgmtListeners {
			m := mgmtListeners[i]
			c := mgmtClusters[i]
			l := listeners.GetByAddress(m.Address)
			if l != nil {
				glog.Warningf("Omitting listener for management address %s (%s) due to collision with service listener %s (%s)",
					m.Name, m.Address, l.Name, l.Address)
				continue
			}
			listeners = append(listeners, m)
			clusters = append(clusters, c)
		}

		// set bind to port values for port redirection
		for _, listener := range listeners {
			listener.BindToPort = false
		}

		// add an extra listener that binds to the port that is the recipient of the iptables redirect
		listeners = append(listeners, &Listener{
			Name:           VirtualListenerName,
			Address:        fmt.Sprintf("tcp://%s:%d", WildcardAddress, mesh.ProxyListenPort),
			BindToPort:     true,
			UseOriginalDst: true,
			Filters:        make([]*NetworkFilter, 0),
		})
	}

	// enable HTTP PROXY port if necessary; this will add an RDS route for this port
	if mesh.ProxyHttpPort > 0 {
		useRemoteAddress := false
		traceOperation := EgressTraceOperation
		listenAddress := LocalhostAddress

		if node.Type == proxy.Router {
			useRemoteAddress = true
			traceOperation = IngressTraceOperation
			listenAddress = WildcardAddress
		}

		// only HTTP outbound clusters are needed
		httpOutbound := buildOutboundHTTPRoutes(mesh, node, instances, services, config)
		httpOutbound = buildEgressHTTPRoutes(mesh, node, instances, config, httpOutbound)
		clusters = append(clusters,
			httpOutbound.clusters()...)
		listeners = append(listeners,
			buildHTTPListener(mesh, node, instances, nil, listenAddress, int(mesh.ProxyHttpPort),
				RDSAll, useRemoteAddress, traceOperation, config))
		// TODO: need inbound listeners in HTTP_PROXY case, with dedicated ingress listener.
	}

	return listeners.normalize(), clusters.normalize()
}

// buildRDSRoutes supplies RDS-enabled HTTP routes
// The route name is assumed to be the port number used by the route in the
// listener, or the special value for _all routes_.
// TODO: this can be optimized by querying for a specific HTTP port in the table
func buildRDSRoute(mesh *proxyconfig.MeshConfig, node proxy.Node, routeName string,
	discovery model.ServiceDiscovery, config model.IstioConfigStore) (*HTTPRouteConfig, error) {
	var httpConfigs HTTPRouteConfigs
	switch node.Type {
	case proxy.Ingress:
		instances, err := discovery.HostInstances(map[string]bool{node.IPAddress: true})
		if err != nil {
			return nil, err
		}
		httpConfigs, _ = buildIngressRoutes(mesh, instances, discovery, config)
	case proxy.Sidecar, proxy.Router:
		instances, err := discovery.HostInstances(map[string]bool{node.IPAddress: true})
		if err != nil {
			return nil, err
		}
		services, err := discovery.Services()
		if err != nil {
			return nil, err
		}
		httpConfigs = buildOutboundHTTPRoutes(mesh, node, instances, services, config)
		httpConfigs = buildEgressHTTPRoutes(mesh, node, instances, config, httpConfigs)
	default:
		return nil, errors.New("unrecognized node type")
	}

	if routeName == RDSAll {
		return httpConfigs.combine(), nil
	}

	port, err := strconv.Atoi(routeName)
	if err != nil {
		return nil, err
	}

	return httpConfigs[port], nil
}

// buildHTTPListener constructs a listener for the network interface address and port.
// Set RDS parameter to a non-empty value to enable RDS for the matching route name.
func buildHTTPListener(mesh *proxyconfig.MeshConfig, node proxy.Node, instances []*model.ServiceInstance,
	routeConfig *HTTPRouteConfig, ip string, port int, rds string, useRemoteAddress bool, direction string,
	store model.IstioConfigStore) *Listener {
	filters := buildFaultFilters(routeConfig)

	filters = append(filters, HTTPFilter{
		Type:   decoder,
		Name:   router,
		Config: FilterRouterConfig{},
	})

	filter := HTTPFilter{
		Name:   CORSFilter,
		Config: CORSFilterConfig{},
	}
	filters = append([]HTTPFilter{filter}, filters...)

	if mesh.MixerAddress != "" {
		mixerConfig := mixerHTTPRouteConfig(node, instances, store)
		filter := HTTPFilter{
			Type:   decoder,
			Name:   MixerFilter,
			Config: mixerConfig,
		}
		filters = append([]HTTPFilter{filter}, filters...)
	}

	config := &HTTPFilterConfig{
		CodecType:        auto,
		UseRemoteAddress: useRemoteAddress,
		StatPrefix:       "http",
		Filters:          filters,
	}

	if mesh.AccessLogFile != "" {
		config.AccessLog = []AccessLog{{
			Path: mesh.AccessLogFile,
		}}
	}

	if mesh.EnableTracing {
		config.GenerateRequestID = true
		config.Tracing = &HTTPFilterTraceConfig{
			OperationName: direction,
		}
	}

	if rds != "" {
		config.RDS = &RDS{
			Cluster:         RDSName,
			RouteConfigName: rds,
			RefreshDelayMs:  protoDurationToMS(mesh.RdsRefreshDelay),
		}
	} else {
		config.RouteConfig = routeConfig
	}

	return &Listener{
		BindToPort: true,
		Name:       fmt.Sprintf("http_%s_%d", ip, port),
		Address:    fmt.Sprintf("tcp://%s:%d", ip, port),
		Filters: []*NetworkFilter{{
			Type:   read,
			Name:   HTTPConnectionManager,
			Config: config,
		}},
	}
}

// consolidateAuthPolicy returns service auth policy, if it's not INHERIT. Else,
// returns mesh policy.
func consolidateAuthPolicy(mesh *proxyconfig.MeshConfig, serviceAuthPolicy proxyconfig.AuthenticationPolicy) proxyconfig.AuthenticationPolicy {
	if serviceAuthPolicy != proxyconfig.AuthenticationPolicy_INHERIT {
		return serviceAuthPolicy
	}
	// TODO: use AuthenticationPolicy for mesh policy and remove this conversion
	switch mesh.AuthPolicy {
	case proxyconfig.MeshConfig_MUTUAL_TLS:
		return proxyconfig.AuthenticationPolicy_MUTUAL_TLS
	case proxyconfig.MeshConfig_NONE:
		return proxyconfig.AuthenticationPolicy_NONE
	default:
		// Never get here, there are no other enum value for mesh.AuthPolicy.
		panic(fmt.Sprintf("Unknown mesh auth policy: %v\n", mesh.AuthPolicy))
	}
}

// mayApplyInboundAuth adds ssl_context to the listener if consolidateAuthPolicy.
func mayApplyInboundAuth(listener *Listener, mesh *proxyconfig.MeshConfig,
	serviceAuthPolicy proxyconfig.AuthenticationPolicy) {
	if consolidateAuthPolicy(mesh, serviceAuthPolicy) == proxyconfig.AuthenticationPolicy_MUTUAL_TLS {
		listener.SSLContext = buildListenerSSLContext(proxy.AuthCertsPath)
	}
}

// buildTCPListener constructs a listener for the TCP proxy
// in addition, it enables mongo proxy filter based on the protocol
func buildTCPListener(tcpConfig *TCPRouteConfig, ip string, port int, protocol model.Protocol) *Listener {

	baseTCPProxy := &NetworkFilter{
		Type: read,
		Name: TCPProxyFilter,
		Config: &TCPProxyFilterConfig{
			StatPrefix:  "tcp",
			RouteConfig: tcpConfig,
		},
	}

	switch protocol {
	case model.ProtocolMongo:
		// TODO: add a watcher for /var/lib/istio/mongo/certs
		// if certs are found use, TLS or mTLS clusters for talking to MongoDB.
		// User is responsible for mounting those certs in the pod.
		return &Listener{
			Name:    fmt.Sprintf("mongo_%s_%d", ip, port),
			Address: fmt.Sprintf("tcp://%s:%d", ip, port),
			Filters: []*NetworkFilter{{
				Type: both,
				Name: MongoProxyFilter,
				Config: &MongoProxyFilterConfig{
					StatPrefix: "mongo",
				},
			},
				baseTCPProxy,
			},
		}
	case model.ProtocolRedis:
		// Redis filter requires the cluster name to be specified
		// as part of the filter. We extract the cluster from the
		// TCPRoute. Since TCPRoute has only one route, we take the
		// cluster from the first route. The moment this route array
		// has multiple routes, we need a fallback. For the moment,
		// fallback to base TCP.

		// Unlike Mongo, Redis is a standalone filter, that is not
		// stacked on top of tcp_proxy
		if len(tcpConfig.Routes) == 1 {
			return &Listener{
				Name:    fmt.Sprintf("redis_%s_%d", ip, port),
				Address: fmt.Sprintf("tcp://%s:%d", ip, port),
				Filters: []*NetworkFilter{{
					Type: both,
					Name: RedisProxyFilter,
					Config: &RedisProxyFilterConfig{
						ClusterName: tcpConfig.Routes[0].Cluster,
						StatPrefix:  "redis",
						ConnPool: &RedisConnPool{
							OperationTimeoutMS: int64(RedisDefaultOpTimeout / time.Millisecond),
						},
					},
				}},
			}
		}
	}

	return &Listener{
		Name:    fmt.Sprintf("tcp_%s_%d", ip, port),
		Address: fmt.Sprintf("tcp://%s:%d", ip, port),
		Filters: []*NetworkFilter{baseTCPProxy},
	}
}

// buildOutboundListeners combines HTTP routes and TCP listeners
func buildOutboundListeners(mesh *proxyconfig.MeshConfig, sidecar proxy.Node, instances []*model.ServiceInstance,
	services []*model.Service, config model.IstioConfigStore) (Listeners, Clusters) {
	listeners, clusters := buildOutboundTCPListeners(mesh, sidecar, services)

	egressTCPListeners, egressTCPClusters := buildEgressTCPListeners(mesh, sidecar, config)
	listeners = append(listeners, egressTCPListeners...)
	clusters = append(clusters, egressTCPClusters...)

	// note that outbound HTTP routes are supplied through RDS
	httpOutbound := buildOutboundHTTPRoutes(mesh, sidecar, instances, services, config)
	httpOutbound = buildEgressHTTPRoutes(mesh, sidecar, instances, config, httpOutbound)

	for port, routeConfig := range httpOutbound {
		operation := EgressTraceOperation
		useRemoteAddress := false

		if sidecar.Type == proxy.Router {
			// if this is in Router mode, then use ingress style trace operation, and remote address settings
			useRemoteAddress = true
			operation = IngressTraceOperation
		}

		l := buildHTTPListener(mesh, sidecar, instances, routeConfig, WildcardAddress, port,
			fmt.Sprintf("%d", port), useRemoteAddress, operation, config)
		listeners = append(listeners, l)
		clusters = append(clusters, routeConfig.clusters()...)
	}

	return listeners, clusters
}

// buildDestinationHTTPRoutes creates HTTP route for a service and a port from rules
func buildDestinationHTTPRoutes(service *model.Service,
	servicePort *model.Port,
	instances []*model.ServiceInstance,
	config model.IstioConfigStore) []*HTTPRoute {
	protocol := servicePort.Protocol
	switch protocol {
	case model.ProtocolHTTP, model.ProtocolHTTP2, model.ProtocolGRPC:
		routes := make([]*HTTPRoute, 0)

		// collect route rules
		useDefaultRoute := true
		rules := config.RouteRules(instances, service.Hostname)
		// sort for output uniqueness
		model.SortRouteRules(rules)
		for _, rule := range rules {
			httpRoute := buildHTTPRoute(rule, service, servicePort)
			routes = append(routes, httpRoute)

			// User can provide timeout/retry policies without any match condition,
			// or specific route. User could also provide a single default route, in
			// which case, we should not be generating another default route.
			// For every HTTPRoute we build, the return value also provides a boolean
			// "catchAll" flag indicating if the route that was built was a catch all route.
			// When such a route is encountered, we stop building further routes for the
			// destination and we will not add the default route after the for loop.
			if httpRoute.CatchAll() {
				useDefaultRoute = false
				break
			}
		}

		if useDefaultRoute {
			// default route for the destination is always the lowest priority route
			cluster := buildOutboundCluster(service.Hostname, servicePort, nil)
			routes = append(routes, buildDefaultRoute(cluster))
		}

		return routes

	case model.ProtocolHTTPS:
		// as an exception, external name HTTPS port is sent in plain-text HTTP/1.1
		if service.External() {
			cluster := buildOutboundCluster(service.Hostname, servicePort, nil)
			return []*HTTPRoute{buildDefaultRoute(cluster)}
		}

	case model.ProtocolTCP, model.ProtocolMongo, model.ProtocolRedis:
		// handled by buildOutboundTCPListeners

	default:
		glog.V(4).Infof("Unsupported outbound protocol %v for port %#v", protocol, servicePort)
	}

	return nil
}

// buildOutboundHTTPRoutes creates HTTP route configs indexed by ports for the
// traffic outbound from the proxy instance
func buildOutboundHTTPRoutes(mesh *proxyconfig.MeshConfig, sidecar proxy.Node,
	instances []*model.ServiceInstance, services []*model.Service, config model.IstioConfigStore) HTTPRouteConfigs {
	httpConfigs := make(HTTPRouteConfigs)
	suffix := strings.Split(sidecar.Domain, ".")

	// outbound connections/requests are directed to service ports; we create a
	// map for each service port to define filters
	for _, service := range services {
		for _, servicePort := range service.Ports {
			routes := buildDestinationHTTPRoutes(service, servicePort, instances, config)

			if len(routes) > 0 {
				host := buildVirtualHost(service, servicePort, suffix, routes)
				http := httpConfigs.EnsurePort(servicePort.Port)

				// there should be at most one occurrence of the service for the same
				// port since service port values are distinct; that means the virtual
				// host domains, which include the sole domain name for the service, do
				// not overlap for the same route config.
				// for example, a service "a" with two ports 80 and 8080, would have virtual
				// hosts on 80 and 8080 listeners that contain domain "a".
				http.VirtualHosts = append(http.VirtualHosts, host)
			}
		}
	}

	return httpConfigs.normalize()
}

// buildOutboundTCPListeners lists listeners and referenced clusters for TCP
// protocols (including HTTPS)
//
// TODO(github.com/istio/pilot/issues/237)
//
// Sharing tcp_proxy and http_connection_manager filters on the same port for
// different destination services doesn't work with Envoy (yet). When the
// tcp_proxy filter's route matching fails for the http service the connection
// is closed without falling back to the http_connection_manager.
//
// Temporary workaround is to add a listener for each service IP that requires
// TCP routing
//
// Connections to the ports of non-load balanced services are directed to
// the connection's original destination. This avoids costly queries of instance
// IPs and ports, but requires that ports of non-load balanced service be unique.
func buildOutboundTCPListeners(mesh *proxyconfig.MeshConfig, sidecar proxy.Node,
	services []*model.Service) (Listeners, Clusters) {
	tcpListeners := make(Listeners, 0)
	tcpClusters := make(Clusters, 0)

	var originalDstCluster *Cluster
	wildcardListenerPorts := make(map[int]bool)
	for _, service := range services {
		if service.External() {
			continue // TODO TCP external services not currently supported
		}
		for _, servicePort := range service.Ports {
			switch servicePort.Protocol {
			case model.ProtocolTCP, model.ProtocolHTTPS, model.ProtocolMongo, model.ProtocolRedis:
				if service.LoadBalancingDisabled || service.Address == "" ||
					sidecar.Type == proxy.Router {
					// ensure only one wildcard listener is created per port if its headless service
					// or if its for a Router (where there is one wildcard TCP listener per port)
					// or if this is in environment where services don't get a dummy load balancer IP.
					if wildcardListenerPorts[servicePort.Port] {
						glog.V(4).Infof("Multiple definitions for port %d", servicePort.Port)
						continue
					}
					wildcardListenerPorts[servicePort.Port] = true

					var cluster *Cluster
					// Router mode cannot handle headless services
					if service.LoadBalancingDisabled && sidecar.Type != proxy.Router {
						if originalDstCluster == nil {
							originalDstCluster = buildOriginalDSTCluster(
								"orig-dst-cluster-tcp", mesh.ConnectTimeout)
							tcpClusters = append(tcpClusters, originalDstCluster)
						}
						cluster = originalDstCluster
					} else {
						cluster = buildOutboundCluster(service.Hostname, servicePort, nil)
						tcpClusters = append(tcpClusters, cluster)
					}
					route := buildTCPRoute(cluster, nil)
					config := &TCPRouteConfig{Routes: []*TCPRoute{route}}
					listener := buildTCPListener(
						config, WildcardAddress, servicePort.Port, servicePort.Protocol)
					if sidecar.Type == proxy.Router {
						listener.BindToPort = true
					}
					tcpListeners = append(tcpListeners, listener)
				} else {
					cluster := buildOutboundCluster(service.Hostname, servicePort, nil)
					route := buildTCPRoute(cluster, []string{service.Address})
					config := &TCPRouteConfig{Routes: []*TCPRoute{route}}
					listener := buildTCPListener(
						config, service.Address, servicePort.Port, servicePort.Protocol)
					tcpClusters = append(tcpClusters, cluster)
					tcpListeners = append(tcpListeners, listener)
				}
			}
		}
	}

	return tcpListeners, tcpClusters
}

// buildInboundListeners creates listeners for the server-side (inbound)
// configuration for co-located service instances. The function also returns
// all inbound clusters since they are statically declared in the proxy
// configuration and do not utilize CDS.
func buildInboundListeners(mesh *proxyconfig.MeshConfig, sidecar proxy.Node,
	instances []*model.ServiceInstance, config model.IstioConfigStore) (Listeners, Clusters) {
	listeners := make(Listeners, 0, len(instances))
	clusters := make(Clusters, 0, len(instances))

	// inbound connections/requests are redirected to the endpoint address but appear to be sent
	// to the service address
	// assumes that endpoint addresses/ports are unique in the instance set
	// TODO: validate that duplicated endpoints for services can be handled (e.g. above assumption)
	for _, instance := range instances {
		endpoint := instance.Endpoint
		servicePort := endpoint.ServicePort
		protocol := servicePort.Protocol
		cluster := buildInboundCluster(endpoint.Port, protocol, mesh.ConnectTimeout)
		clusters = append(clusters, cluster)

		var listener *Listener

		// Local service instances can be accessed through one of three
		// addresses: localhost, endpoint IP, and service
		// VIP. Localhost bypasses the proxy and doesn't need any TCP
		// route config. Endpoint IP is handled below and Service IP is handled
		// by outbound routes.
		// Traffic sent to our service VIP is redirected by remote
		// services' kubeproxy to our specific endpoint IP.
		switch protocol {
		case model.ProtocolHTTP, model.ProtocolHTTP2, model.ProtocolGRPC:
			defaultRoute := buildDefaultRoute(cluster)

			// set server-side mixer filter config for inbound HTTP routes
			if mesh.MixerAddress != "" {
				defaultRoute.OpaqueConfig = buildMixerOpaqueConfig(!mesh.DisablePolicyChecks, false,
					instance.Service.Hostname)
			}

			host := &VirtualHost{
				Name:    fmt.Sprintf("inbound|%d", endpoint.Port),
				Domains: []string{"*"},
				Routes:  []*HTTPRoute{},
			}

			// Websocket enabled routes need to have an explicit use_websocket : true
			// This setting needs to be enabled on Envoys at both sender and receiver end
			if protocol == model.ProtocolHTTP {
				// get all the route rules applicable to the instances
				rules := config.RouteRulesByDestination(instances)
				// sort for the output uniqueness
				model.SortRouteRules(rules)
				for _, config := range rules {
					rule := config.Spec.(*proxyconfig.RouteRule)
					if route := buildInboundRoute(config, rule, cluster); route != nil {
						// set server-side mixer filter config for inbound HTTP routes
						// Note: websocket routes do not call the filter chain. Will be
						// resolved in future.
						if mesh.MixerAddress != "" {
							route.OpaqueConfig = buildMixerOpaqueConfig(!mesh.DisablePolicyChecks, false,
								instance.Service.Hostname)
						}

						host.Routes = append(host.Routes, route)
					}
				}
			}

			host.Routes = append(host.Routes, defaultRoute)

			routeConfig := &HTTPRouteConfig{VirtualHosts: []*VirtualHost{host}}
			listener = buildHTTPListener(mesh, sidecar, instances, routeConfig, endpoint.Address,
				endpoint.Port, "", false, IngressTraceOperation, config)

		case model.ProtocolTCP, model.ProtocolHTTPS, model.ProtocolMongo, model.ProtocolRedis:
			listener = buildTCPListener(&TCPRouteConfig{
				Routes: []*TCPRoute{buildTCPRoute(cluster, []string{endpoint.Address})},
			}, endpoint.Address, endpoint.Port, protocol)

			// set server-side mixer filter config
			if mesh.MixerAddress != "" {
				filter := &NetworkFilter{
					Type:   both,
					Name:   MixerFilter,
					Config: mixerTCPConfig(sidecar, !mesh.DisablePolicyChecks, instance),
				}
				listener.Filters = append([]*NetworkFilter{filter}, listener.Filters...)
			}

		default:
			glog.V(4).Infof("Unsupported inbound protocol %v for port %#v", protocol, servicePort)
		}

		if listener != nil {
			mayApplyInboundAuth(listener, mesh, endpoint.ServicePort.AuthenticationPolicy)
			listeners = append(listeners, listener)
		}
	}

	return listeners, clusters
}

func appendPortToDomains(domains []string, port int) []string {
	domainsWithPorts := make([]string, len(domains), 2*len(domains))
	copy(domainsWithPorts, domains)

	for _, domain := range domains {
		domainsWithPorts = append(domainsWithPorts, domain+":"+strconv.Itoa(port))
	}

	return domainsWithPorts
}

func truncateClusterName(name string) string {
	if len(name) > MaxClusterNameLength {
		prefix := name[:MaxClusterNameLength-sha1.Size*2]
		sum := sha1.Sum([]byte(name))
		return fmt.Sprintf("%s%x", prefix, sum)
	}
	return name
}

func buildEgressVirtualHost(rule *proxyconfig.EgressRule,
	mesh *proxyconfig.MeshConfig, port *model.Port, instances []*model.ServiceInstance,
	config model.IstioConfigStore) *VirtualHost {
	var externalTrafficCluster *Cluster
	destination := rule.Destination.Service

	protocolToHandle := port.Protocol
	if protocolToHandle == model.ProtocolGRPC {
		protocolToHandle = model.ProtocolHTTP2
	}

	// Create a unique orig dst cluster for each service defined by egress rule
	// So that we can apply circuit breakers, outlier detections, etc., later.
	svc := model.Service{Hostname: destination}
	key := svc.Key(port, nil)
	name := truncateClusterName(key)
	externalTrafficCluster = buildOriginalDSTCluster(name, mesh.ConnectTimeout)
	externalTrafficCluster.ServiceName = key
	externalTrafficCluster.hostname = destination
	externalTrafficCluster.port = port
	if protocolToHandle == model.ProtocolHTTPS {
		externalTrafficCluster.SSLContext = &SSLContextExternal{}
	}

	if protocolToHandle == model.ProtocolHTTP2 {
		externalTrafficCluster.Features = ClusterFeatureHTTP2
	}

	if protocolToHandle == model.ProtocolHTTPS {
		// temporarily set the protocol to HTTP because we require applications
		// to use http to talk to external services (and we do TLS origination).
		// buildDestinationHTTPRoutes does not generate route blocks for HTTPS services
		port.Protocol = model.ProtocolHTTP
	}

	routes := buildDestinationHTTPRoutes(&model.Service{Hostname: destination}, port, instances, config)
	// reset the protocol to the original value
	port.Protocol = protocolToHandle

	if len(routes) > 0 {
		// Set the destination clusters to the cluster we computed above.
		// Services defined via egress rules do not have labels and hence no weighted clusters
		for _, route := range routes {
			route.Cluster = externalTrafficCluster.Name
			route.clusters = []*Cluster{externalTrafficCluster}
		}
	}

	virtualHostName := destination + ":" + strconv.Itoa(port.Port)
	return &VirtualHost{
		Name:    virtualHostName,
		Domains: appendPortToDomains([]string{destination}, port.Port),
		Routes:  routes,
	}
}

func buildEgressHTTPRoutes(mesh *proxyconfig.MeshConfig, node proxy.Node,
	instances []*model.ServiceInstance, config model.IstioConfigStore,
	httpConfigs HTTPRouteConfigs) HTTPRouteConfigs {

	if node.Type == proxy.Router {
		// No egress rule support for Routers. As semantics are not clear.
		return httpConfigs
	}

	egressRules, errs := model.RejectConflictingEgressRules(config.EgressRules())

	if errs != nil {
		glog.Warningf("Rejected rules: %v", errs)
	}

	for _, rule := range egressRules {
		for _, port := range rule.Ports {
			protocol := model.ConvertCaseInsensitiveStringToProtocol(port.Protocol)
			if protocol != model.ProtocolHTTP && protocol != model.ProtocolHTTPS &&
				protocol != model.ProtocolHTTP2 && protocol != model.ProtocolGRPC {
				continue
			}
			intPort := int(port.Port)
			modelPort := &model.Port{Name: fmt.Sprintf("external-%v-%d", protocol, intPort),
				Port: intPort, Protocol: protocol}
			httpConfig := httpConfigs.EnsurePort(intPort)
			httpConfig.VirtualHosts = append(httpConfig.VirtualHosts,
				buildEgressVirtualHost(rule, mesh, modelPort, instances, config))
		}
	}

	return httpConfigs.normalize()
}

// buildEgressTCPListeners builds a listener on 0.0.0.0 per each distinct port of all TCP egress
// rules and a cluster per each TCP egress rule
func buildEgressTCPListeners(mesh *proxyconfig.MeshConfig, node proxy.Node,
	config model.IstioConfigStore) (Listeners, Clusters) {

	tcpListeners := make(Listeners, 0)
	tcpClusters := make(Clusters, 0)

	if node.Type == proxy.Router {
		// No egress rule support for Routers. As semantics are not clear.
		return tcpListeners, tcpClusters
	}

	egressRules, errs := model.RejectConflictingEgressRules(config.EgressRules())

	if errs != nil {
		glog.Warningf("Rejected rules: %v", errs)
	}

	tcpRulesByPort := make(map[int][]*proxyconfig.EgressRule)
	tcpProtocolByPort := make(map[int]model.Protocol)

	for _, rule := range egressRules {
		for _, port := range rule.Ports {
			protocol := model.Protocol(strings.ToUpper(port.Protocol))
			if !model.IsEgressRulesSupportedTCPProtocol(protocol) {
				continue
			}
			intPort := int(port.Port)
			tcpRulesByPort[intPort] = append(tcpRulesByPort[intPort], rule)
			tcpProtocolByPort[intPort] = protocol
		}
	}

	for intPort, rules := range tcpRulesByPort {
		protocol := tcpProtocolByPort[intPort]
		modelPort := &model.Port{Name: fmt.Sprintf("external-%v-%d", protocol, intPort),
			Port: intPort, Protocol: protocol}

		tcpRoutes := make([]*TCPRoute, 0)
		for _, rule := range rules {
			tcpRoute, tcpCluster := buildEgressTCPRoute(rule, mesh, modelPort)
			tcpRoutes = append(tcpRoutes, tcpRoute)
			tcpClusters = append(tcpClusters, tcpCluster)
		}

		config := &TCPRouteConfig{Routes: tcpRoutes}
		tcpListener := buildTCPListener(config, WildcardAddress, intPort, protocol)
		tcpListeners = append(tcpListeners, tcpListener)
	}

	return tcpListeners, tcpClusters
}

// buildEgressTCPRoute builds a tcp route and a cluster per port of a TCP egress service
// see comment to buildOutboundTCPListeners
func buildEgressTCPRoute(rule *proxyconfig.EgressRule,
	mesh *proxyconfig.MeshConfig, port *model.Port) (*TCPRoute, *Cluster) {

	// Create a unique orig dst cluster for each service defined by egress rule
	// So that we can apply circuit breakers, outlier detections, etc., later.
	destination := rule.Destination.Service
	svc := model.Service{Hostname: destination}
	key := svc.Key(port, nil)
	name := truncateClusterName(key)
	externalTrafficCluster := buildOriginalDSTCluster(name, mesh.ConnectTimeout)
	externalTrafficCluster.port = port
	externalTrafficCluster.ServiceName = key
	externalTrafficCluster.hostname = destination

	route := buildTCPRoute(externalTrafficCluster, []string{destination})
	return route, externalTrafficCluster
}

// buildMgmtPortListeners creates inbound TCP only listeners for the management ports on
// server (inbound). The function also returns all inbound clusters since
// they are statically declared in the proxy configuration and do not
// utilize CDS.
// Management port listeners are slightly different from standard Inbound listeners
// in that, they do not have mixer filters nor do they have inbound auth.
// N.B. If a given management port is same as the service instance's endpoint port
// the pod will fail to start in Kubernetes, because the mixer service tries to
// lookup the service associated with the Pod. Since the pod is yet to be started
// and hence not bound to the service), the service lookup fails causing the mixer
// to fail the health check call. This results in a vicious cycle, where kubernetes
// restarts the unhealthy pod after successive failed health checks, and the mixer
// continues to reject the health checks as there is no service associated with
// the pod.
// So, if a user wants to use kubernetes probes with Istio, she should ensure
// that the health check ports are distinct from the service ports.
func buildMgmtPortListeners(mesh *proxyconfig.MeshConfig, managementPorts model.PortList,
	managementIP string) (Listeners, Clusters) {
	listeners := make(Listeners, 0, len(managementPorts))
	clusters := make(Clusters, 0, len(managementPorts))

	// assumes that inbound connections/requests are sent to the endpoint address
	for _, mPort := range managementPorts {
		switch mPort.Protocol {
		case model.ProtocolHTTP, model.ProtocolHTTP2, model.ProtocolGRPC, model.ProtocolTCP,
			model.ProtocolHTTPS, model.ProtocolMongo, model.ProtocolRedis:
			cluster := buildInboundCluster(mPort.Port, model.ProtocolTCP, mesh.ConnectTimeout)
			listener := buildTCPListener(&TCPRouteConfig{
				Routes: []*TCPRoute{buildTCPRoute(cluster, []string{managementIP})},
			}, managementIP, mPort.Port, model.ProtocolTCP)

			clusters = append(clusters, cluster)
			listeners = append(listeners, listener)
		default:
			glog.Warningf("Unsupported inbound protocol %v for management port %#v",
				mPort.Protocol, mPort)
		}
	}

	return listeners, clusters
}
