package envoy

import (
	"context"
	"log"
	"net"
	"regexp"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	dynamic_forward_cluster "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/dynamic_forward_proxy/v3"
	dynamic_forward_filter "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_forward_proxy/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/dynamic_forward_proxy/v3"
	http "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	xds "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/networkop/envoy-split-proxy/pkg/config"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

const prefix = "envoy-split-proxy"

var (
	defaultClusterName       = prefix + "-default"
	defaultTLSCluster        = defaultClusterName + "-tls"
	defaultHTTPCluster       = defaultClusterName + "-http"
	bypassClusterName        = prefix + "-bypass"
	bypassTLSCluster         = bypassClusterName + "-tls"
	bypassHTTPCluster        = bypassClusterName + "-http"
	defaultHTTPSListenerName = prefix + "-https-listener"
	defaultHTTPListenerName  = prefix + "-http-listener"
	partialWildCard          = regexp.MustCompile(`\*\.[^\d]+`)
)

// Envoy stores the XDS server configuration
type Envoy struct {
	cache     cache.SnapshotCache
	nodeID    string
	httpsPort int
	httpPort  int
}

// NewServer creates a new XDS server
func NewServer(grpcURL string, nodeID string, httpsPort, httpPort int) (*Envoy, error) {

	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)

	server := xds.NewServer(context.Background(), snapshotCache, nil)

	grpcServer := grpc.NewServer()
	lis, err := net.Listen("tcp", grpcURL)
	if err != nil {
		return nil, err
	}

	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, server)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("Failed to initialize grpc server: %s\n", err)
		}
	}()

	return &Envoy{
		cache:     snapshotCache,
		nodeID:    nodeID,
		httpsPort: httpsPort,
		httpPort:  httpPort,
	}, nil
}

// Configure applies the desired state to the proxy
func (e *Envoy) Configure(in chan *config.Data) {

	for d := range in {
		logrus.Infof("Received new config: %+v", d)

		cluster := buildCluster(d.IP.String())
		route := makeRoute(d.URLs)
		listener := buildListener(d.URLs, e.httpsPort, e.httpPort)
		//snapshot := cache.NewSnapshot(time.Now().String(), nil, cluster, nil, listener, nil, nil)
		snapshot, _ := cache.NewSnapshot(time.Now().String(),map[resource.Type][]types.Resource{
			resource.RouteType:    {route},
			resource.ClusterType:  cluster,
			resource.ListenerType: listener,
		})
		err := e.cache.SetSnapshot(context.Background(),e.nodeID, snapshot)
		if err != nil {
			logrus.Infof("Failed to update envoy config: %s", err)
		} else {
			d.Changed = false
		}

	}
}

func buildCluster(ip string) []types.Resource {
	defaultTLSCluster := newEnvoyTLSCluster(defaultTLSCluster)
	defaultHTTPCluster := newEnvoyHTTPCluster(defaultHTTPCluster)

	bypassTLSCluster := newEnvoyTLSCluster(bypassTLSCluster)
	bypassHTTPCluster := newEnvoyHTTPCluster(bypassHTTPCluster)
	bypassTLSCluster.UpstreamBindConfig = &core.BindConfig{
		SourceAddress: &core.SocketAddress{
			Address: ip,
			PortSpecifier: &core.SocketAddress_PortValue{
				PortValue: uint32(0),
			},
		},
	}
	bypassHTTPCluster.UpstreamBindConfig = &core.BindConfig{
		SourceAddress: &core.SocketAddress{
			Address: ip,
			PortSpecifier: &core.SocketAddress_PortValue{
				PortValue: uint32(0),
			},
		},
	}

	return []types.Resource{defaultTLSCluster, bypassTLSCluster, defaultHTTPCluster, bypassHTTPCluster}
}
func newEnvoyTLSCluster(name string) *cluster.Cluster {
	logrus.Debugf("Creating Envoy TLS cluster %s", name)
	return &cluster.Cluster{
		Name:                 name,
		ConnectTimeout:       ptypes.DurationProto(5 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_ORIGINAL_DST},
		DnsLookupFamily:      cluster.Cluster_V4_ONLY,
		LbPolicy:             cluster.Cluster_CLUSTER_PROVIDED,
	}
}

func newEnvoyHTTPCluster(name string) *cluster.Cluster {
	logrus.Debugf("Creating Envoy HTTP cluster %s", name)
	return &cluster.Cluster{
		Name:           name,
		ConnectTimeout: ptypes.DurationProto(5 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_ClusterType{
			ClusterType: &cluster.Cluster_CustomClusterType{
				Name: "envoy.clusters.dynamic_forward_proxy",
				TypedConfig: makeAny(&dynamic_forward_cluster.ClusterConfig{
					DnsCacheConfig: &v3.DnsCacheConfig{
						Name:            "dns",
						DnsLookupFamily: cluster.Cluster_V4_ONLY,
					},
				}),
			},
		},
		DnsLookupFamily: cluster.Cluster_V4_ONLY,
		LbPolicy:        cluster.Cluster_CLUSTER_PROVIDED,
	}
}
func buildListener(urls []string, httpsPort, httpPort int) []types.Resource {
	return []types.Resource{
		&listener.Listener{
			Name: defaultHTTPListenerName,
			Address: &core.Address{
				Address: &core.Address_SocketAddress{
					SocketAddress: &core.SocketAddress{
						Address: "0.0.0.0",
						PortSpecifier: &core.SocketAddress_PortValue{
							PortValue: uint32(httpPort),
						},
					},
				},
			},
			FilterChains: []*listener.FilterChain{
				{
					Filters: []*listener.Filter{
						{
							Name: wellknown.HTTPConnectionManager,
							ConfigType: &listener.Filter_TypedConfig{
								TypedConfig: makeAny(&http.HttpConnectionManager{
									//CodecType:  http.HttpConnectionManager_AUTO,
									//NormalizePath:     &wrappers.BoolValue{Value: true},
									//MergeSlashes:      true,
									//GenerateRequestId: &wrappers.BoolValue{Value: false},
									StreamIdleTimeout: &duration.Duration{Seconds: 300},
									StatPrefix:        prefix,
									HttpFilters: []*http.HttpFilter{
										{
											Name: "http-filter",
											ConfigType: &http.HttpFilter_TypedConfig{
												TypedConfig: makeAny(&dynamic_forward_filter.FilterConfig{
													DnsCacheConfig: &v3.DnsCacheConfig{
														Name:            "dns",
														DnsLookupFamily: cluster.Cluster_V4_ONLY,
													},
												}),
											},
										},
										&http.HttpFilter{
											Name:       wellknown.Router,
											ConfigType: nil,
										},
									},
									RouteSpecifier: &http.HttpConnectionManager_Rds{
										Rds: &http.Rds{
											ConfigSource:    makeConfigSource(),
											RouteConfigName: "default",
										},
									},
								}),
							},
						},
					},
				},
			},
		},
		&listener.Listener{
			Name: defaultHTTPSListenerName,
			Address: &core.Address{
				Address: &core.Address_SocketAddress{
					SocketAddress: &core.SocketAddress{
						Address: "0.0.0.0",
						PortSpecifier: &core.SocketAddress_PortValue{
							PortValue: uint32(httpsPort),
						},
					},
				},
			},
			FilterChains: []*listener.FilterChain{
				{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: excludePartialWildCards(urls),
					},
					Filters: []*listener.Filter{
						{
							Name: wellknown.TCPProxy,
							ConfigType: &listener.Filter_TypedConfig{
								TypedConfig: newClusterTypedConfig(bypassTLSCluster),
							},
						},
					},
				},
				{
					Filters: []*listener.Filter{
						{
							Name: wellknown.TCPProxy,
							ConfigType: &listener.Filter_TypedConfig{
								TypedConfig: newClusterTypedConfig(defaultTLSCluster),
							},
						},
					},
				},
			},
		},
	}
}
func makeRoute(urls []string) *route.RouteConfiguration {
	return &route.RouteConfiguration{
		Name: "default",
		VirtualHosts: []*route.VirtualHost{
			{
				Name:    "default",
				Domains: []string{"*"},
				Routes: []*route.Route{
					{
						Match: &route.RouteMatch{
							PathSpecifier: &route.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &route.Route_Route{
							Route: &route.RouteAction{
								ClusterSpecifier: &route.RouteAction_Cluster{
									Cluster: defaultHTTPCluster,
								},
							},
						},
					},
				},
			},
			{
				Name:    "default-bypass",
				Domains: urls,
				Routes: []*route.Route{
					{
						Match: &route.RouteMatch{
							PathSpecifier: &route.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &route.Route_Route{
							Route: &route.RouteAction{
								ClusterSpecifier: &route.RouteAction_Cluster{
									Cluster: bypassHTTPCluster,
								},
							},
						},
					},
				},
			},
		},
	}
}

func newClusterTypedConfig(name string) *any.Any {
	logrus.Debugf("Building cluster config for %s", name)

	cluster := &tcp_proxy.TcpProxy{
		StatPrefix: prefix,
		ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{
			Cluster: name,
		},
	}

	config, err := ptypes.MarshalAny(cluster)
	if err != nil {
		logrus.Infof("Failed to build the listener config: %s", err)
	}
	return config
}

func makeAny(pb proto.Message) *any.Any {
	any, err := ptypes.MarshalAny(pb)
	if err != nil {
		panic(err.Error())
	}
	return any
}

func excludePartialWildCards(urls []string) []string {
	var output []string
	for _, url := range urls {
		if partialWildCard.MatchString(url) {
			output = append(output, url)
		}

	}
	return output
}

func makeConfigSource() *core.ConfigSource {
	source := &core.ConfigSource{}
	source.ResourceApiVersion = resource.DefaultAPIVersion
	source.ConfigSourceSpecifier = &core.ConfigSource_ApiConfigSource{
		ApiConfigSource: &core.ApiConfigSource{
			TransportApiVersion:       resource.DefaultAPIVersion,
			ApiType:                   core.ApiConfigSource_GRPC,
			SetNodeOnFirstMessageOnly: true,
			GrpcServices: []*core.GrpcService{{
				TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: "xds_cluster"},
				},
			}},
		},
	}
	return source
}
