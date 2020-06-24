// Copyright Istio Authors
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

package v1alpha3

import (
	"fmt"
	"sort"
	"strings"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/hashicorp/go-multierror"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/pkg/log"

	meshconfig "istio.io/api/mesh/v1alpha1"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	istionetworking "istio.io/istio/pilot/pkg/networking"
	istio_route "istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
	authn_model "istio.io/istio/pilot/pkg/security/model"
	"istio.io/istio/pkg/config/gateway"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/proto"
)

func (configgen *ConfigGeneratorImpl) buildGatewayListeners(
	node *model.Proxy,
	push *model.PushContext,
	builder *ListenerBuilder) *ListenerBuilder {

	if node.MergedGateway == nil {
		log.Debuga("buildGatewayListeners: no gateways for router ", node.ID)
		return builder
	}

	mergedGateway := node.MergedGateway
	log.Debugf("buildGatewayListeners: gateways after merging: %v", mergedGateway)

	actualWildcard, _ := getActualWildcardAndLocalHost(node)
	errs := &multierror.Error{}
	listeners := make([]*listener.Listener, 0, len(mergedGateway.Servers))
	proxyConfig := node.Metadata.ProxyConfigOrDefault(push.Mesh.DefaultConfig)
	for portNumber, servers := range mergedGateway.Servers {
		var si *model.ServiceInstance
		services := make(map[host.Name]struct{}, len(node.ServiceInstances))
		for _, w := range node.ServiceInstances {
			if w.ServicePort.Port == int(portNumber) {
				if si == nil {
					si = w
				}
				services[w.Service.Hostname] = struct{}{}
			}
		}
		if len(services) != 1 {
			log.Warnf("buildGatewayListeners: found %d services on port %d: %v",
				len(services), portNumber, services)
		}
		// if we found a ServiceInstance with matching ServicePort, listen on TargetPort
		if si != nil && si.Endpoint != nil {
			portNumber = si.Endpoint.EndpointPort
		}
		// on a given port, we can either have plain text HTTP servers or
		// HTTPS/TLS servers with SNI. We cannot have a mix of http and https server on same port.
		opts := buildListenerOpts{
			push:       push,
			proxy:      node,
			bind:       actualWildcard,
			port:       int(portNumber),
			bindToPort: true,
		}

		p := protocol.Parse(servers[0].Port.Protocol)
		listenerProtocol := istionetworking.ModelProtocolToListenerProtocol(p, core.TrafficDirection_OUTBOUND)
		filterChains := make([]istionetworking.FilterChain, 0)
		if p.IsHTTP() {
			// We have a list of HTTP servers on this port. Build a single listener for the server port.
			// We only need to look at the first server in the list as the merge logic
			// ensures that all servers are of same type.
			routeName := mergedGateway.RouteNamesByServer[servers[0]]
			opts.filterChainOpts = []*filterChainOpts{configgen.createGatewayHTTPFilterChainOpts(node, servers[0], routeName, "", proxyConfig)}
			filterChains = append(filterChains, istionetworking.FilterChain{ListenerProtocol: istionetworking.ListenerProtocolHTTP})
		} else {
			// build http connection manager with TLS context, for HTTPS servers using simple/mutual TLS
			// build listener with tcp proxy, with or without TLS context, for TCP servers
			//   or TLS servers using simple/mutual/passthrough TLS
			//   or HTTPS servers using passthrough TLS
			// This process typically yields multiple filter chain matches (with SNI) [if TLS is used]
			filterChainOpts := make([]*filterChainOpts, 0)

			for _, server := range servers {
				if gateway.IsTLSServer(server) && gateway.IsHTTPServer(server) {
					// This is a HTTPS server, where we are doing TLS termination. Build a http connection manager with TLS context
					routeName := mergedGateway.RouteNamesByServer[server]
					filterChainOpts = append(filterChainOpts, configgen.createGatewayHTTPFilterChainOpts(node, server, routeName, push.Mesh.SdsUdsPath, proxyConfig))
					filterChains = append(filterChains, istionetworking.FilterChain{ListenerProtocol: istionetworking.ListenerProtocolHTTP})
				} else {
					// passthrough or tcp, yields multiple filter chains
					tcpChainOpts := configgen.createGatewayTCPFilterChainOpts(node, push,
						server, map[string]bool{mergedGateway.GatewayNameForServer[server]: true})
					filterChainOpts = append(filterChainOpts, tcpChainOpts...)
					for i := 0; i < len(tcpChainOpts); i++ {
						filterChains = append(filterChains, istionetworking.FilterChain{ListenerProtocol: istionetworking.ListenerProtocolTCP})
					}
				}
			}
			opts.filterChainOpts = filterChainOpts
		}

		l := buildListener(opts)
		l.TrafficDirection = core.TrafficDirection_OUTBOUND

		mutable := &istionetworking.MutableObjects{
			Listener: l,
			// Note: buildListener creates filter chains but does not populate the filters in the chain; that's what
			// this is for.
			FilterChains: filterChains,
		}

		pluginParams := &plugin.InputParams{
			ListenerProtocol: listenerProtocol,
			Node:             node,
			Push:             push,
			ServiceInstance:  si,
			Port: &model.Port{
				Name:     servers[0].Port.Name,
				Port:     int(portNumber),
				Protocol: p,
			},
		}
		for _, p := range configgen.Plugins {
			if err := p.OnOutboundListener(pluginParams, mutable); err != nil {
				log.Warna("buildGatewayListeners: failed to build listener for gateway: ", err.Error())
			}
		}

		// Filters are serialized one time into an opaque struct once we have the complete list.
		if err := buildCompleteFilterChain(pluginParams, mutable, opts); err != nil {
			errs = multierror.Append(errs, fmt.Errorf("gateway omitting listener %q due to: %v", mutable.Listener.Name, err.Error()))
			continue
		}

		if err := mutable.Listener.Validate(); err != nil {
			errs = multierror.Append(errs, fmt.Errorf("gateway listener %s validation failed: %v", mutable.Listener.Name, err.Error()))
			continue
		}

		if log.DebugEnabled() {
			log.Debugf("buildGatewayListeners: constructed listener with %d filter chains:\n%v",
				len(mutable.Listener.FilterChains), mutable.Listener)
		}
		listeners = append(listeners, mutable.Listener)
	}
	// We'll try to return any listeners we successfully marshaled; if we have none, we'll emit the error we built up
	err := errs.ErrorOrNil()
	if err != nil {
		// we have some listeners to return, but we also have some errors; log them
		log.Info(err.Error())
	}

	if len(listeners) == 0 {
		log.Error("buildGatewayListeners: Have zero listeners")
		return builder
	}

	validatedListeners := make([]*listener.Listener, 0, len(mergedGateway.Servers))
	for _, l := range listeners {
		if err := l.Validate(); err != nil {
			log.Warnf("buildGatewayListeners: error validating listener %s: %v.. Skipping.", l.Name, err)
			continue
		}
		validatedListeners = append(validatedListeners, l)
	}

	builder.gatewayListeners = validatedListeners
	return builder
}

func (configgen *ConfigGeneratorImpl) buildGatewayHTTPRouteConfig(node *model.Proxy, push *model.PushContext,
	routeName string) *route.RouteConfiguration {

	services := push.Services(node)

	if node.MergedGateway == nil {
		log.Debuga("buildGatewayRoutes: no gateways for router ", node.ID)
		return nil
	}

	merged := node.MergedGateway
	log.Debugf("buildGatewayRoutes: gateways after merging: %v", merged)

	// make sure that there is some server listening on this port
	if _, ok := merged.ServersByRouteName[routeName]; !ok {
		log.Warnf("Gateway missing for route %s. This is normal if gateway was recently deleted. Have %v", routeName, merged.ServersByRouteName)

		// This can happen when a gateway has recently been deleted. Envoy will still request route
		// information due to the draining of listeners, so we should not return an error.
		return nil
	}

	servers := merged.ServersByRouteName[routeName]
	port := int(servers[0].Port.Number) // all these servers are for the same routeName, and therefore same port

	nameToServiceMap := make(map[host.Name]*model.Service, len(services))
	for _, svc := range services {
		nameToServiceMap[svc.Hostname] = svc
	}

	vHostDedupMap := make(map[host.Name]*route.VirtualHost)
	for _, server := range servers {
		gatewayName := merged.GatewayNameForServer[server]
		virtualServices := push.VirtualServices(node, map[string]bool{gatewayName: true})
		for _, virtualService := range virtualServices {
			virtualServiceHosts := host.NewNames(virtualService.Spec.(*networking.VirtualService).Hosts)
			serverHosts := host.NamesForNamespace(server.Hosts, virtualService.Namespace)

			// We have two cases here:
			// 1. virtualService hosts are 1.foo.com, 2.foo.com, 3.foo.com and server hosts are ns/*.foo.com
			// 2. virtualService hosts are *.foo.com, and server hosts are ns/1.foo.com, ns/2.foo.com, ns/3.foo.com
			intersectingHosts := serverHosts.Intersection(virtualServiceHosts)
			if len(intersectingHosts) == 0 {
				continue
			}

			routes, err := istio_route.BuildHTTPRoutesForVirtualService(node, push, virtualService, nameToServiceMap, port, map[string]bool{gatewayName: true})
			if err != nil {
				log.Debugf("%s omitting routes for service %v due to error: %v", node.ID, virtualService, err)
				continue
			}

			for _, hostname := range intersectingHosts {
				if vHost, exists := vHostDedupMap[hostname]; exists {
					vHost.Routes = istio_route.CombineVHostRoutes(vHost.Routes, routes)
				} else {
					newVHost := &route.VirtualHost{
						Name:                       domainName(string(hostname), port),
						Domains:                    buildGatewayVirtualHostDomains(string(hostname)),
						Routes:                     routes,
						IncludeRequestAttemptCount: true,
					}
					if server.Tls != nil && server.Tls.HttpsRedirect {
						newVHost.RequireTls = route.VirtualHost_ALL
					}
					vHostDedupMap[hostname] = newVHost
				}
			}
		}
	}

	var virtualHosts []*route.VirtualHost
	if len(vHostDedupMap) == 0 {
		log.Warnf("constructed http route config for route %s on port %d with no vhosts; Setting up a default 404 vhost", routeName, port)
		virtualHosts = []*route.VirtualHost{{
			Name:    domainName("blackhole", port),
			Domains: []string{"*"},
			Routes: []*route.Route{
				{
					Match: &route.RouteMatch{
						PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"},
					},
					Action: &route.Route_DirectResponse{
						DirectResponse: &route.DirectResponseAction{
							Status: 404,
						},
					},
				},
			},
		}}
		// add a name to the route
		virtualHosts[0].Routes[0].Name = istio_route.DefaultRouteName
	} else {
		virtualHosts = make([]*route.VirtualHost, 0, len(vHostDedupMap))
		for _, v := range vHostDedupMap {
			virtualHosts = append(virtualHosts, v)
		}
	}

	util.SortVirtualHosts(virtualHosts)

	routeCfg := &route.RouteConfiguration{
		// Retain the routeName as its used by EnvoyFilter patching logic
		Name:             routeName,
		VirtualHosts:     virtualHosts,
		ValidateClusters: proto.BoolFalse,
	}

	in := &plugin.InputParams{
		ListenerProtocol: istionetworking.ListenerProtocolHTTP,
		ListenerCategory: networking.EnvoyFilter_GATEWAY,
		Node:             node,
		Push:             push,
	}

	// call plugins
	for _, p := range configgen.Plugins {
		p.OnOutboundRouteConfiguration(in, routeCfg)
	}

	return routeCfg
}

// builds a HTTP connection manager for servers of type HTTP or HTTPS (mode: simple/mutual)
func (configgen *ConfigGeneratorImpl) createGatewayHTTPFilterChainOpts(
	node *model.Proxy, server *networking.Server, routeName string,
	sdsPath string, proxyConfig *meshconfig.ProxyConfig) *filterChainOpts {

	serverProto := protocol.Parse(server.Port.Protocol)

	httpProtoOpts := &core.Http1ProtocolOptions{}

	if features.HTTP10 || node.Metadata.HTTP10 == "1" {
		httpProtoOpts.AcceptHttp_10 = true
	}

	xffNumTrustedHops := uint32(0)
	forwardClientCertDetails := util.MeshConfigToEnvoyForwardClientCertDetails(meshconfig.Topology_SANITIZE_SET)

	if proxyConfig != nil && proxyConfig.GatewayTopology != nil {
		xffNumTrustedHops = proxyConfig.GatewayTopology.NumTrustedProxies
		if proxyConfig.GatewayTopology.ForwardClientCertDetails != meshconfig.Topology_UNDEFINED {
			forwardClientCertDetails = util.MeshConfigToEnvoyForwardClientCertDetails(proxyConfig.GatewayTopology.ForwardClientCertDetails)
		}
	}

	// Are we processing plaintext servers or HTTPS servers?
	// If plain text, we have to combine all servers into a single listener
	if serverProto.IsHTTP() {
		return &filterChainOpts{
			// This works because we validate that only HTTPS servers can have same port but still different port names
			// and that no two non-HTTPS servers can be on same port or share port names.
			// Validation is done per gateway and also during merging
			sniHosts:   nil,
			tlsContext: nil,
			httpOpts: &httpListenerOpts{
				rds:              routeName,
				useRemoteAddress: true,
				connectionManager: &hcm.HttpConnectionManager{
					XffNumTrustedHops: xffNumTrustedHops,
					// Forward client cert if connection is mTLS
					ForwardClientCertDetails: forwardClientCertDetails,
					SetCurrentClientCertDetails: &hcm.HttpConnectionManager_SetCurrentClientCertDetails{
						Subject: proto.BoolTrue,
						Cert:    true,
						Uri:     true,
						Dns:     true,
					},
					ServerName:          EnvoyServerName,
					HttpProtocolOptions: httpProtoOpts,
				},
				addGRPCWebFilter: serverProto == protocol.GRPCWeb,
			},
		}
	}

	// Build a filter chain for the HTTPS server
	// We know that this is a HTTPS server because this function is called only for ports of type HTTP/HTTPS
	// where HTTPS server's TLS mode is not passthrough and not nil
	return &filterChainOpts{
		// This works because we validate that only HTTPS servers can have same port but still different port names
		// and that no two non-HTTPS servers can be on same port or share port names.
		// Validation is done per gateway and also during merging
		sniHosts:   getSNIHostsForServer(server),
		tlsContext: buildGatewayListenerTLSContext(server, sdsPath, node.Metadata),
		httpOpts: &httpListenerOpts{
			rds:              routeName,
			useRemoteAddress: true,
			connectionManager: &hcm.HttpConnectionManager{
				XffNumTrustedHops: xffNumTrustedHops,
				// Forward client cert if connection is mTLS
				ForwardClientCertDetails: forwardClientCertDetails,
				SetCurrentClientCertDetails: &hcm.HttpConnectionManager_SetCurrentClientCertDetails{
					Subject: proto.BoolTrue,
					Cert:    true,
					Uri:     true,
					Dns:     true,
				},
				ServerName:          EnvoyServerName,
				HttpProtocolOptions: httpProtoOpts,
			},
		},
	}
}

// sdsPath: is the path to the mesh-wide workload sds uds path, and it is assumed that if this path is unset, that sds is
// disabled mesh-wide
// metadata: map of miscellaneous configuration values sent from the Envoy instance back to Pilot, could include the field
//
// Below is a table of potential scenarios for the gateway configuration:
//
// TLS mode      | Mesh-wide SDS | Ingress SDS | Resulting Configuration
// SIMPLE/MUTUAL |    ENABLED    |   ENABLED   | support SDS at ingress gateway to terminate SSL communication outside the mesh
// ISTIO_MUTUAL  |    ENABLED    |   DISABLED  | support SDS at gateway to terminate workload mTLS, with internal workloads
// 											   | for egress or with another trusted cluster for ingress)
// ISTIO_MUTUAL  |    DISABLED   |   DISABLED  | use file-mounted secret paths to terminate workload mTLS from gateway
//
// Note that ISTIO_MUTUAL TLS mode and ingressSds should not be used simultaneously on the same ingress gateway.
func buildGatewayListenerTLSContext(
	server *networking.Server, sdsPath string, metadata *model.NodeMetadata) *tls.DownstreamTlsContext {
	// Server.TLS cannot be nil or passthrough. But as a safety guard, return nil
	if server.Tls == nil || gateway.IsPassThroughServer(server) {
		return nil // We don't need to setup TLS context for passthrough mode
	}

	ctx := &tls.DownstreamTlsContext{
		CommonTlsContext: &tls.CommonTlsContext{
			AlpnProtocols: util.ALPNHttp,
		},
	}

	if server.Tls.CredentialName != "" {
		// If SDS is enabled at gateway, and credential name is specified at gateway config, create
		// SDS config for gateway to fetch key/cert at gateway agent.
		authn_model.ApplyCustomSDSToCommonTLSContext(ctx.CommonTlsContext, server.Tls, authn_model.IngressGatewaySdsUdsPath)
	} else if server.Tls.Mode == networking.ServerTLSSettings_ISTIO_MUTUAL {
		authn_model.ApplyToCommonTLSContext(ctx.CommonTlsContext, metadata, sdsPath, server.Tls.SubjectAltNames)
	} else {
		// Fall back to the read-from-file approach when SDS is not enabled or Tls.CredentialName is not specified.
		ctx.CommonTlsContext.TlsCertificates = []*tls.TlsCertificate{
			{
				CertificateChain: &core.DataSource{
					Specifier: &core.DataSource_Filename{
						Filename: server.Tls.ServerCertificate,
					},
				},
				PrivateKey: &core.DataSource{
					Specifier: &core.DataSource_Filename{
						Filename: server.Tls.PrivateKey,
					},
				},
			},
		}
		var trustedCa *core.DataSource
		if len(server.Tls.CaCertificates) != 0 {
			trustedCa = &core.DataSource{
				Specifier: &core.DataSource_Filename{
					Filename: server.Tls.CaCertificates,
				},
			}
		}
		if trustedCa != nil || len(server.Tls.SubjectAltNames) > 0 {
			ctx.CommonTlsContext.ValidationContextType = &tls.CommonTlsContext_ValidationContext{
				ValidationContext: &tls.CertificateValidationContext{
					TrustedCa:            trustedCa,
					MatchSubjectAltNames: util.StringToExactMatch(server.Tls.SubjectAltNames),
				},
			}
		}
	}

	ctx.RequireClientCertificate = proto.BoolFalse
	if server.Tls.Mode == networking.ServerTLSSettings_MUTUAL ||
		server.Tls.Mode == networking.ServerTLSSettings_ISTIO_MUTUAL {
		ctx.RequireClientCertificate = proto.BoolTrue
	}

	// Set TLS parameters if they are non-default
	if len(server.Tls.CipherSuites) > 0 ||
		server.Tls.MinProtocolVersion != networking.ServerTLSSettings_TLS_AUTO ||
		server.Tls.MaxProtocolVersion != networking.ServerTLSSettings_TLS_AUTO {

		ctx.CommonTlsContext.TlsParams = &tls.TlsParameters{
			TlsMinimumProtocolVersion: convertTLSProtocol(server.Tls.MinProtocolVersion),
			TlsMaximumProtocolVersion: convertTLSProtocol(server.Tls.MaxProtocolVersion),
			CipherSuites:              server.Tls.CipherSuites,
		}
	}

	return ctx
}

func convertTLSProtocol(in networking.ServerTLSSettings_TLSProtocol) tls.TlsParameters_TlsProtocol {
	out := tls.TlsParameters_TlsProtocol(in) // There should be a one-to-one enum mapping
	if out < tls.TlsParameters_TLS_AUTO || out > tls.TlsParameters_TLSv1_3 {
		log.Warnf("was not able to map TLS protocol to Envoy TLS protocol")
		return tls.TlsParameters_TLS_AUTO
	}
	return out
}

func (configgen *ConfigGeneratorImpl) createGatewayTCPFilterChainOpts(
	node *model.Proxy, push *model.PushContext, server *networking.Server,
	gatewaysForWorkload map[string]bool) []*filterChainOpts {

	// We have a TCP/TLS server. This could be TLS termination (user specifies server.TLS with simple/mutual)
	// or opaque TCP (server.TLS is nil). or it could be a TLS passthrough with SNI based routing.

	// This is opaque TCP server. Find matching virtual services with TCP blocks and forward
	if server.Tls == nil {
		if filters := buildGatewayNetworkFiltersFromTCPRoutes(node,
			push, server, gatewaysForWorkload); len(filters) > 0 {
			return []*filterChainOpts{
				{
					sniHosts:       nil,
					tlsContext:     nil,
					networkFilters: filters,
				},
			}
		}
	} else if !gateway.IsPassThroughServer(server) {
		// TCP with TLS termination and forwarding. Setup TLS context to terminate, find matching services with TCP blocks
		// and forward to backend
		// Validation ensures that non-passthrough servers will have certs
		if filters := buildGatewayNetworkFiltersFromTCPRoutes(node,
			push, server, gatewaysForWorkload); len(filters) > 0 {
			return []*filterChainOpts{
				{
					sniHosts:       getSNIHostsForServer(server),
					tlsContext:     buildGatewayListenerTLSContext(server, push.Mesh.SdsUdsPath, node.Metadata),
					networkFilters: filters,
				},
			}
		}
	} else {
		// Passthrough server.
		return buildGatewayNetworkFiltersFromTLSRoutes(node, push, server, gatewaysForWorkload)
	}

	return []*filterChainOpts{}
}

// buildGatewayNetworkFiltersFromTCPRoutes builds tcp proxy routes for all VirtualServices with TCP blocks.
// It first obtains all virtual services bound to the set of Gateways for this workload, filters them by this
// server's port and hostnames, and produces network filters for each destination from the filtered services.
func buildGatewayNetworkFiltersFromTCPRoutes(node *model.Proxy, push *model.PushContext, server *networking.Server,
	gatewaysForWorkload map[string]bool) []*listener.Filter {
	port := &model.Port{
		Name:     server.Port.Name,
		Port:     int(server.Port.Number),
		Protocol: protocol.Parse(server.Port.Protocol),
	}

	gatewayServerHosts := make(map[host.Name]bool, len(server.Hosts))
	for _, hostname := range server.Hosts {
		gatewayServerHosts[host.Name(hostname)] = true
	}

	virtualServices := push.VirtualServices(node, gatewaysForWorkload)
	for _, v := range virtualServices {
		vsvc := v.Spec.(*networking.VirtualService)
		// We have two cases here:
		// 1. virtualService hosts are 1.foo.com, 2.foo.com, 3.foo.com and gateway's hosts are ns/*.foo.com
		// 2. virtualService hosts are *.foo.com, and gateway's hosts are ns/1.foo.com, ns/2.foo.com, ns/3.foo.com
		// Since this is TCP, neither matters. We are simply looking for matching virtual service for this gateway
		matchingHosts := pickMatchingGatewayHosts(gatewayServerHosts, v)
		if len(matchingHosts) == 0 {
			// the VirtualService's hosts don't include hosts advertised by server
			continue
		}

		// ensure we satisfy the rule's l4 match conditions, if any exist
		// For the moment, there can be only one match that succeeds
		// based on the match port/server port and the gateway name
		for _, tcp := range vsvc.Tcp {
			if l4MultiMatch(tcp.Match, server, gatewaysForWorkload) {
				return buildOutboundNetworkFilters(node, tcp.Route, push, port, v.ConfigMeta)
			}
		}
	}

	return nil
}

// buildGatewayNetworkFiltersFromTLSRoutes builds tcp proxy routes for all VirtualServices with TLS blocks.
// It first obtains all virtual services bound to the set of Gateways for this workload, filters them by this
// server's port and hostnames, and produces network filters for each destination from the filtered services
func buildGatewayNetworkFiltersFromTLSRoutes(node *model.Proxy, push *model.PushContext, server *networking.Server,
	gatewaysForWorkload map[string]bool) []*filterChainOpts {
	port := &model.Port{
		Name:     server.Port.Name,
		Port:     int(server.Port.Number),
		Protocol: protocol.Parse(server.Port.Protocol),
	}

	gatewayServerHosts := make(map[host.Name]bool, len(server.Hosts))
	for _, hostname := range server.Hosts {
		gatewayServerHosts[host.Name(hostname)] = true
	}

	filterChains := make([]*filterChainOpts, 0)

	if server.Tls.Mode == networking.ServerTLSSettings_AUTO_PASSTHROUGH {
		// auto passthrough does not require virtual services. It sets up envoy.filters.network.sni_cluster filter
		filterChains = append(filterChains, &filterChainOpts{
			sniHosts:       getSNIHostsForServer(server),
			tlsContext:     nil, // NO TLS context because this is passthrough
			networkFilters: buildOutboundAutoPassthroughFilterStack(push, node, port),
		})
	} else {
		virtualServices := push.VirtualServices(node, gatewaysForWorkload)
		for _, v := range virtualServices {
			vsvc := v.Spec.(*networking.VirtualService)
			// We have two cases here:
			// 1. virtualService hosts are 1.foo.com, 2.foo.com, 3.foo.com and gateway's hosts are ns/*.foo.com
			// 2. virtualService hosts are *.foo.com, and gateway's hosts are ns/1.foo.com, ns/2.foo.com, ns/3.foo.com
			// The code below only handles 1.
			// TODO: handle case 2
			matchingHosts := pickMatchingGatewayHosts(gatewayServerHosts, v)
			if len(matchingHosts) == 0 {
				// the VirtualService's hosts don't include hosts advertised by server
				continue
			}

			// For every matching TLS block, generate a filter chain with sni match
			// TODO: Bug..if there is a single virtual service with *.foo.com, and multiple TLS block
			// matches, one for 1.foo.com, another for 2.foo.com, this code will produce duplicate filter
			// chain matches
			for _, tls := range vsvc.Tls {
				for _, match := range tls.Match {
					if l4SingleMatch(convertTLSMatchToL4Match(match), server, gatewaysForWorkload) {
						// the sni hosts in the match will become part of a filter chain match
						filterChains = append(filterChains, &filterChainOpts{
							sniHosts:       match.SniHosts,
							tlsContext:     nil, // NO TLS context because this is passthrough
							networkFilters: buildOutboundNetworkFilters(node, tls.Route, push, port, v.ConfigMeta),
						})
					}
				}
			}
		}
	}

	return filterChains
}

// Select the virtualService's hosts that match the ones specified in the gateway server's hosts
// based on the wildcard hostname match and the namespace match
func pickMatchingGatewayHosts(gatewayServerHosts map[host.Name]bool, virtualService model.Config) map[string]host.Name {
	matchingHosts := make(map[string]host.Name)
	virtualServiceHosts := virtualService.Spec.(*networking.VirtualService).Hosts
	for _, vsvcHost := range virtualServiceHosts {
		for gatewayHost := range gatewayServerHosts {
			gwHostnameForMatching := gatewayHost
			if strings.Contains(string(gwHostnameForMatching), "/") {
				// match the namespace first
				// gateway merging code ensures that we only have ns/host
				// and no ./* or */host
				parts := strings.Split(string(gwHostnameForMatching), "/")
				if parts[0] != virtualService.Namespace {
					continue
				}
				//strip the namespace
				gwHostnameForMatching = host.Name(parts[1])
			}
			if gwHostnameForMatching.Matches(host.Name(vsvcHost)) {
				// assign the actual gateway host because calling code uses it as a key
				// to locate TLS redirect servers
				matchingHosts[vsvcHost] = gatewayHost
			}
		}
	}
	return matchingHosts
}

func convertTLSMatchToL4Match(tlsMatch *networking.TLSMatchAttributes) *networking.L4MatchAttributes {
	return &networking.L4MatchAttributes{
		DestinationSubnets: tlsMatch.DestinationSubnets,
		Port:               tlsMatch.Port,
		SourceLabels:       tlsMatch.SourceLabels,
		Gateways:           tlsMatch.Gateways,
		SourceNamespace:    tlsMatch.SourceNamespace,
	}
}

func l4MultiMatch(predicates []*networking.L4MatchAttributes, server *networking.Server, gatewaysForWorkload map[string]bool) bool {
	// NB from proto definitions: each set of predicates is OR'd together; inside of a predicate all conditions are AND'd.
	// This means we can return as soon as we get any match of an entire predicate.
	for _, match := range predicates {
		if l4SingleMatch(match, server, gatewaysForWorkload) {
			return true
		}
	}
	// If we had no predicates we match; otherwise we don't match since we'd have exited at the first match.
	return len(predicates) == 0
}

func l4SingleMatch(match *networking.L4MatchAttributes, server *networking.Server, gatewaysForWorkload map[string]bool) bool {
	// if there's no gateway predicate, gatewayMatch is true; otherwise we match against the gateways for this workload
	return isPortMatch(match.Port, server) && isGatewayMatch(gatewaysForWorkload, match.Gateways)
}

func isPortMatch(port uint32, server *networking.Server) bool {
	// if there's no port predicate, portMatch is true; otherwise we evaluate the port predicate against the server's port
	portMatch := port == 0
	if port != 0 {
		portMatch = server.Port.Number == port
	}
	return portMatch
}

func isGatewayMatch(gatewaysForWorkload map[string]bool, gatewayNames []string) bool {
	// if there's no gateway predicate, gatewayMatch is true; otherwise we match against the gateways for this workload
	gatewayMatch := len(gatewayNames) == 0
	if len(gatewayNames) > 0 {
		for _, gatewayName := range gatewayNames {
			gatewayMatch = gatewayMatch || gatewaysForWorkload[gatewayName]
		}
	}
	return gatewayMatch
}

func getSNIHostsForServer(server *networking.Server) []string {
	if server.Tls == nil {
		return nil
	}
	// sanitize the server hosts as it could contain hosts of form ns/host
	sniHosts := make(map[string]bool)
	for _, h := range server.Hosts {
		if strings.Contains(h, "/") {
			parts := strings.Split(h, "/")
			h = parts[1]
		}
		// do not add hosts, that have already been added
		if !sniHosts[h] {
			sniHosts[h] = true
		}
	}
	sniHostsSlice := make([]string, 0, len(sniHosts))
	for host := range sniHosts {
		sniHostsSlice = append(sniHostsSlice, host)
	}
	sort.Strings(sniHostsSlice)

	return sniHostsSlice
}

func buildGatewayVirtualHostDomains(hostname string) []string {
	domains := []string{hostname}
	if hostname == "*" {
		return domains
	}
	// To support gateway behind a LB with unknown port.
	domains = append(domains, hostname+":*")
	return domains
}
