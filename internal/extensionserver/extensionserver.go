// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extensionserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	egextension "github.com/envoyproxy/gateway/proto/extension"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	extprocv3http "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	upstream_codecv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/upstream_codec/v3"
	httpconnectionmanagerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
)

// Server is the implementation of the EnvoyGatewayExtensionServer interface.
type Server struct {
	egextension.UnimplementedEnvoyGatewayExtensionServer
	log       logr.Logger
	k8sClient client.Client
	// udsPath is the path to the UDS socket.
	// This is used to communicate with the external processor.
	// TODO 用于和extPro通信？
	// 目前只是拿到这个路径, 为envoy配置新增一个cluster, cluster的endpoint是这个uds目录
	udsPath string
}

const serverName = "envoy-gateway-extension-server"

// New creates a new instance of the extension server that implements the EnvoyGatewayExtensionServer interface.
func New(k8sClient client.Client, logger logr.Logger, udsPath string) *Server {
	logger = logger.WithName(serverName)
	// udsPath: /etc/ai-gateway-extproc-uds/run.sock
	return &Server{log: logger, k8sClient: k8sClient, udsPath: udsPath}
}

/*
type HealthServer interface {
	Check(context.Context, *HealthCheckRequest) (*HealthCheckResponse, error)
	List(context.Context, *HealthListRequest) (*HealthListResponse, error)
	Watch(*HealthCheckRequest, grpc.ServerStreamingServer[HealthCheckResponse]) error
}
*/

// Check implements [grpc_health_v1.HealthServer].
// 如上所示三个方法
func (s *Server) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// Watch implements [grpc_health_v1.HealthServer].
func (s *Server) Watch(*grpc_health_v1.HealthCheckRequest, grpc_health_v1.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

// List implements [grpc_health_v1.HealthServer].
func (s *Server) List(context.Context, *grpc_health_v1.HealthListRequest) (*grpc_health_v1.HealthListResponse, error) {
	return &grpc_health_v1.HealthListResponse{Statuses: map[string]*grpc_health_v1.HealthCheckResponse{
		serverName: {Status: grpc_health_v1.HealthCheckResponse_SERVING},
	}}, nil
}

// ExtProcUDSClusterName 新增的cluster名为如下.
const (
	ExtProcUDSClusterName = "ai-gateway-extproc-uds"
)

// PostTranslateModify allows an extension to modify the clusters and secrets in the xDS config.
//
// Currently, this adds an ORIGINAL_DST cluster to the list of clusters unconditionally.
// 可以修改clusters和secret
// kubectl get cm -n envoy-gateway-system envoy-gateway-config -o yaml
/*
apiVersion: v1
data:
  envoy-gateway.yaml: |
    apiVersion: gateway.envoyproxy.io/v1alpha1
    kind: EnvoyGateway
    gateway:
      controllerName: gateway.envoyproxy.io/gatewayclass-controller
    logging:
      level:
        default: info
    provider:
      kubernetes:
        rateLimitDeployment:
          patch:
            type: StrategicMerge
            value:
              spec:
                template:
                  spec:
                    containers:
                    - imagePullPolicy: IfNotPresent
                      name: envoy-ratelimit
                      image: docker.io/envoyproxy/ratelimit:60d8e81b
      type: Kubernetes
    extensionApis:
      enableEnvoyPatchPolicy: true
      enableBackend: true
    extensionManager:
      hooks:
        xdsTranslator:
          post:
            - VirtualHost
            - Translation
      service:
        fqdn:
          hostname: ai-gateway-controller.envoy-ai-gateway-system.svc.cluster.local
          port: 1063
*/
// hook里有Translation,所以实现PostTranslateModify方法
// https://gateway.envoyproxy.io/contributions/design/extending-envoy-gateway/
func (s *Server) PostTranslateModify(_ context.Context, req *egextension.PostTranslateModifyRequest) (*egextension.PostTranslateModifyResponse, error) {
	var extProcUDSExist bool
	for _, cluster := range req.Clusters {
		s.maybeModifyCluster(cluster)
		extProcUDSExist = extProcUDSExist || cluster.Name == ExtProcUDSClusterName
	}
	// 如果不存在,增加一个cluster, 提供extPro的地址
	if !extProcUDSExist {
		po := &httpv3.HttpProtocolOptions{}
		po.UpstreamProtocolOptions = &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
			ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
				Http2ProtocolOptions: &corev3.Http2ProtocolOptions{
					// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/internal/xds/translator/listener.go#L50-L53
					InitialConnectionWindowSize: wrapperspb.UInt32(1048576),
					InitialStreamWindowSize:     wrapperspb.UInt32(65536),
				},
			},
		}}
		req.Clusters = append(req.Clusters, &clusterv3.Cluster{
			Name:                 ExtProcUDSClusterName,
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/api/v1alpha1/timeout_types.go#L25
			ConnectTimeout: &durationpb.Duration{Seconds: 10},
			TypedExtensionProtocolOptions: map[string]*anypb.Any{
				"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustToAny(po),
			},
			// Default is 32768 bytes == 32 KiB which seems small:
			// https://github.com/envoyproxy/gateway/blob/932b8b155fa562ae917da19b497a4370733478f1/internal/xds/translator/cluster.go#L49
			//
			// So, we set it to 50MBi.
			PerConnectionBufferLimitBytes: wrapperspb.UInt32(52428800),
			LoadAssignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: ExtProcUDSClusterName,
				Endpoints: []*endpointv3.LocalityLbEndpoints{
					{
						LbEndpoints: []*endpointv3.LbEndpoint{
							{
								HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
									Endpoint: &endpointv3.Endpoint{
										Address: &corev3.Address{
											Address: &corev3.Address_Pipe{
												Pipe: &corev3.Pipe{
													Path: s.udsPath,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		})
		s.log.Info("Added extproc-uds cluster to the list of clusters")
	}
	response := &egextension.PostTranslateModifyResponse{Clusters: req.Clusters, Secrets: req.Secrets}
	return response, nil
}

// maybeModifyCluster mainly does two things:
//   - Populates the cluster endpoint metadata per backend. This is a workaround until
//     https://github.com/envoyproxy/gateway/issues/5523 as well as the endpoint set level metadata is supported in the extproc.
//   - Insert the upstream external processor filter to the list of filters. https://github.com/envoyproxy/gateway/issues/5881
//   - Insert the header mutation filter to the list of filters.
//
// The result will look almost similar to envoy.yaml in the tests/extproc tests. Please refer to the config file for more details.
func (s *Server) maybeModifyCluster(cluster *clusterv3.Cluster) {
	// The cluster name is in the format "httproute/<namespace>/<name>/rule/<index_of_rule>".
	// We need to extract the namespace and name from the cluster name.
	parts := strings.Split(cluster.Name, "/")
	if len(parts) != 5 || parts[0] != "httproute" {
		s.log.Info("non-ai-gateway cluster name", "cluster_name", cluster.Name)
		return
	}
	httpRouteNamespace := parts[1]
	httpRouteName := parts[2]
	httpRouteRuleIndexStr := parts[4]
	httpRouteRuleIndex, err := strconv.Atoi(httpRouteRuleIndexStr)
	if err != nil {
		s.log.Error(err, "failed to parse HTTPRoute rule index",
			"cluster_name", cluster.Name, "rule_index", httpRouteRuleIndexStr)
		return
	}
	// Get the HTTPRoute object from the cluster name.
	var aigwRoute aigv1a1.AIGatewayRoute
	// 获取aigatewayroute
	err = s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: httpRouteNamespace, Name: httpRouteName}, &aigwRoute)
	if err != nil {
		s.log.Error(err, "failed to get AIGatewayRoute object",
			"namespace", httpRouteNamespace, "name", httpRouteName)
		return
	}
	// Get the backend from the HTTPRoute object.
	// 不匹配无法获取，导致越界访问
	if httpRouteRuleIndex >= len(aigwRoute.Spec.Rules) {
		s.log.Info("HTTPRoute rule index out of range",
			"cluster_name", cluster.Name, "rule_index", httpRouteRuleIndexStr)
		return
	}
	// 通过index判断是哪个rule
	/*
		apiVersion: aigateway.envoyproxy.io/v1alpha1
		kind: AIGatewayRoute
		metadata:
		  name: envoy-ai-gateway-basic
		  namespace: default
		spec:
		  schema:
		    name: OpenAI
		  targetRefs:
		    - name: envoy-ai-gateway-basic
		      kind: Gateway
		      group: gateway.networking.k8s.io
		  rules:
		    - matches:
		        - headers:
		            - type: Exact
		              name: x-ai-eg-model
		              value: gpt-4o-mini
		      backendRefs:
		        - name: envoy-ai-gateway-basic-openai
		    - matches:
		        - headers:
		            - type: Exact
		              name: x-ai-eg-model
		              value: us.meta.llama3-2-1b-instruct-v1:0
		      backendRefs:
		        - name: envoy-ai-gateway-basic-aws
	*/
	httpRouteRule := &aigwRoute.Spec.Rules[httpRouteRuleIndex]
	// cluster没有后端端点
	if cluster.LoadAssignment == nil {
		s.log.Info("LoadAssignment is nil", "cluster_name", cluster.Name)
		return
	}
	// 后端端点的数量和backendRefs的数量不一致，此处的endpoints是LocalityLbEndpoints列表, 可以理解为envoy中的cluster
	// List of endpoints to load balance to
	if len(cluster.LoadAssignment.Endpoints) != len(httpRouteRule.BackendRefs) {
		s.log.Info("LoadAssignment endpoints length does not match backend refs length",
			"cluster_name", cluster.Name, "endpoints_length", len(cluster.LoadAssignment.Endpoints), "backend_refs_length", len(httpRouteRule.BackendRefs))
		return
	}
	// Populate the metadata for each endpoint in the LoadAssignment.
	for i, endpoints := range cluster.LoadAssignment.Endpoints {
		// 一个endpoints是一个envoy中的cluster
		// 取第i个backend,比如envoy-ai-gateway-basic-openai
		backendRef := httpRouteRule.BackendRefs[i]
		name := backendRef.Name
		namespace := aigwRoute.Namespace
		// 如果AIGatewayRoute里配置了priority，则赋值给envoy的endpoints里面
		// 完整的backendRef如下,basic.yaml里面只配置了name
		/*
			type AIGatewayRouteRuleBackendRef struct {
				Name string `json:"name"`
				ModelNameOverride string `json:"modelNameOverride,omitempty"`
				Weight *int32 `json:"weight,omitempty"`
				Priority *uint32 `json:"priority,omitempty"`
			}
		*/
		if backendRef.Priority != nil {
			endpoints.Priority = *backendRef.Priority
		}
		// We populate the same metadata for all endpoints in the LoadAssignment.
		// This is because currently, an extproc cannot retrieve the endpoint set level metadata.
		// LbEndpoints里面保存的是envoy中的endpoint
		for _, endpoint := range endpoints.LbEndpoints {
			if endpoint.Metadata == nil {
				endpoint.Metadata = &corev3.Metadata{}
			}
			if endpoint.Metadata.FilterMetadata == nil {
				endpoint.Metadata.FilterMetadata = make(map[string]*structpb.Struct)
			}
			m, ok := endpoint.Metadata.FilterMetadata["aigateway.envoy.io"]
			if !ok {
				// 如果没有这个元数据，增加一个空的
				m = &structpb.Struct{}
				endpoint.Metadata.FilterMetadata["aigateway.envoy.io"] = m
			}
			if m.Fields == nil {
				m.Fields = make(map[string]*structpb.Value)
			}
			// 设置backend_name为aibackend的name和aigatewayRoute的namespace
			// aibackend的名字和backend的名字一样，所以也可以说是backend的name
			// 这个backend_name会在extproc中用到
			m.Fields["backend_name"] = structpb.NewStringValue(fmt.Sprintf("%s.%s", name, namespace))
		}
	}

	if cluster.TypedExtensionProtocolOptions == nil {
		cluster.TypedExtensionProtocolOptions = make(map[string]*anypb.Any)
	}
	const httpProtocolOptions = "envoy.extensions.upstreams.http.v3.HttpProtocolOptions"
	// 设置httpprotocoloptions
	var po *httpv3.HttpProtocolOptions
	if raw, ok := cluster.TypedExtensionProtocolOptions[httpProtocolOptions]; ok {
		// 如果不是第一次设置,则获取原来的
		po = &httpv3.HttpProtocolOptions{}
		if err = raw.UnmarshalTo(po); err != nil {
			s.log.Error(err, "failed to unmarshal HttpProtocolOptions", "cluster_name", cluster.Name)
			return
		}
	} else {
		// 否则创建一个空的
		po = &httpv3.HttpProtocolOptions{}
		po.UpstreamProtocolOptions = &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
			ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{},
		}}
	}

	// TODO 自己取个名字, 只是唯一标识， 无实际作用？
	const upstreamExtProcNameAIGateway = "envoy.filters.http.ext_proc/aigateway"
	for _, filter := range po.HttpFilters {
		if filter.Name == upstreamExtProcNameAIGateway {
			// Nothing to do, the filter is already there.
			return
		}
	}

	// 新建extProc配置
	extProcConfig := &extprocv3http.ExternalProcessor{}
	extProcConfig.MetadataOptions = &extprocv3http.MetadataOptions{
		ReceivingNamespaces: &extprocv3http.MetadataOptions_MetadataNamespaces{
			// io.envoy.ai_gateway
			Untyped: []string{aigv1a1.AIGatewayFilterMetadataNamespace},
		},
	}
	extProcConfig.AllowModeOverride = true
	// 在 Envoy 中，Attributes（属性）是指在请求处理过程中收集的结构化键值对数据，用于描述请求、响应或系统状态
	/*
		内置：
		request.method         # HTTP 请求方法
		request.path           # 请求路径
		request.headers        # 请求头
		response.status_code   # 响应状态码
		connection.remote_ip   # 客户端 IP

		自定义：
		myapp.user_id          # 用户 ID
		myapp.auth_role        # 认证角色

		动态生成：
		envoy.filters.http.jwt_authn.claims  # JWT 解析后的声明
		envoy.filters.http.rbac.effect       # 访问控制结果
	*/
	// xds.upstream_host_metadata: 上游集群节点,通常用于负载均衡，节点选择
	extProcConfig.RequestAttributes = []string{"xds.upstream_host_metadata"}
	// 设置请求、响应头，请求、响应body模式
	// 需要传请求头到extproc，不需要传请求body到extproc
	// 不需要传响应header到extpro，也不需要传响应body到extproc
	extProcConfig.ProcessingMode = &extprocv3http.ProcessingMode{
		RequestHeaderMode: extprocv3http.ProcessingMode_SEND,
		// At the upstream filter, it can access the original body in its memory, so it can perform the translation
		// as well as the authentication at the request headers. Hence, there's no need to send the request body to the extproc.
		RequestBodyMode: extprocv3http.ProcessingMode_NONE,
		// Response will be handled at the router filter level so that we could avoid the shenanigans around the retry+the upstream filter.
		// Do not send the header or trailer
		ResponseHeaderMode: extprocv3http.ProcessingMode_SKIP,
		ResponseBodyMode:   extprocv3http.ProcessingMode_NONE,
	}
	// Configuration for the gRPC service that the filter will communicate with
	// filter需要与哪个grpc服务端进行通信
	// 已经append到了cluster里面，cluster里面定义了地址，就是/etc/ai-gateway-extproc-uds/run.sock
	extProcConfig.GrpcService = &corev3.GrpcService{
		TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
			EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
				// ai-gateway-extproc-uds
				ClusterName: ExtProcUDSClusterName,
			},
		},
		Timeout: durationpb.New(30 * time.Second),
	}
	// 新建一个httpFilter，名字叫 envoy.filters.http.ext_proc/aigateway
	// 配置为上面的配置
	extProcFilter := &httpconnectionmanagerv3.HttpFilter{
		Name:       upstreamExtProcNameAIGateway,
		ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{TypedConfig: mustToAny(extProcConfig)},
	}

	// 新建一个httpFilter，作用是修改header
	headerMutFilter := &httpconnectionmanagerv3.HttpFilter{
		Name: "envoy.filters.http.header_mutation",
		ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{
			TypedConfig: mustToAny(&header_mutationv3.HeaderMutation{
				Mutations: &header_mutationv3.Mutations{
					RequestMutations: []*mutation_rulesv3.HeaderMutation{
						{
							Action: &mutation_rulesv3.HeaderMutation_Append{
								Append: &corev3.HeaderValueOption{
									// 添加header
									AppendAction: corev3.HeaderValueOption_ADD_IF_ABSENT,
									// 把content-length加进来
									Header: &corev3.HeaderValue{
										Key:   "content-length",
										Value: `%DYNAMIC_METADATA(` + aigv1a1.AIGatewayFilterMetadataNamespace + `:content_length)%`,
									},
								},
							},
						},
					},
				},
			}),
		},
	}

	// 即原来已存在
	if len(po.HttpFilters) > 0 {
		// Insert the ext_proc filter before the last filter since the last one is always the upstream codec filter.
		// 最后一个filter,始终是upstream codec filter
		last := po.HttpFilters[len(po.HttpFilters)-1]
		// 效果如下：
		// [pres, extProcFilter, headerMutFilter, last]
		po.HttpFilters = po.HttpFilters[:len(po.HttpFilters)-1]
		po.HttpFilters = append(po.HttpFilters, extProcFilter, headerMutFilter, last)
	} else {
		// 新设置filter
		// 效果如下：
		// [pres, extProcFilter, headerMutFilter, last]
		// 因为codec始终要在最后，所以要新建一个
		po.HttpFilters = append(po.HttpFilters, extProcFilter, headerMutFilter)
		// We always need the upstream_code filter as a last filter.
		upstreamCodec := &httpconnectionmanagerv3.HttpFilter{}
		upstreamCodec.Name = "envoy.filters.http.upstream_codec"
		upstreamCodec.ConfigType = &httpconnectionmanagerv3.HttpFilter_TypedConfig{
			TypedConfig: mustToAny(&upstream_codecv3.UpstreamCodec{}),
		}
		po.HttpFilters = append(po.HttpFilters, upstreamCodec)
	}
	// 更新cluster.httpProtocolOptions
	cluster.TypedExtensionProtocolOptions[httpProtocolOptions] = mustToAny(po)
}

func mustToAny(msg proto.Message) *anypb.Any {
	b, err := proto.Marshal(msg)
	if err != nil {
		panic(fmt.Sprintf("BUG: failed to marshal message: %v", err))
	}
	const envoyAPIPrefix = "type.googleapis.com/"
	return &anypb.Any{
		TypeUrl: envoyAPIPrefix + string(msg.ProtoReflect().Descriptor().FullName()),
		Value:   b,
	}
}

// PostVirtualHostModify allows an extension to modify the virtual hosts in the xDS config.
func (s *Server) PostVirtualHostModify(context.Context, *egextension.PostVirtualHostModifyRequest) (*egextension.PostVirtualHostModifyResponse, error) {
	return nil, nil
}
