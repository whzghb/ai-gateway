// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"cmp"
	"context"
	"fmt"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/yaml"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/controller/rotators"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
)

const (
	// FilterConfigKeyInSecret is the key to store the filter config in the secret.
	FilterConfigKeyInSecret = "filter-config.yaml" //nolint: gosec
	// defaultOwnedBy is the default value for the ModelsOwnedBy field in the filter config.
	defaultOwnedBy = "Envoy AI Gateway"
)

// NewGatewayController creates a new reconcile.TypedReconciler for gwapiv1.Gateway.
//
// extProcImage is the image of the external processor sidecar container which will be used
// to check if the pods of the gateway deployment need to be rolled out.
func NewGatewayController(
	client client.Client, kube kubernetes.Interface, logger logr.Logger,
	envoyGatewayNamespace, udsPath, extProcImage string,
) *GatewayController {
	return &GatewayController{
		client:                client,
		kube:                  kube,
		logger:                logger,
		envoyGatewayNamespace: envoyGatewayNamespace,
		// /etc/ai-gateway-extproc-uds/run.sock 和extPro路径一致
		udsPath:      udsPath,
		extProcImage: extProcImage,
	}
}

// GatewayController implements reconcile.TypedReconciler for gwapiv1.Gateway.
type GatewayController struct {
	client                client.Client
	kube                  kubernetes.Interface
	logger                logr.Logger
	envoyGatewayNamespace string
	udsPath               string
	extProcImage          string // The image of the external processor sidecar container.
}

// Reconcile implements the reconcile.Reconciler for gwapiv1.Gateway.
func (c *GatewayController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var gw gwapiv1.Gateway
	if err := c.client.Get(ctx, req.NamespacedName, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var routes aigv1a1.AIGatewayRouteList
	// 通过gateway快速获取与该gateway绑定的所有AIGatewayRoute
	err := c.client.List(ctx, &routes, client.MatchingFields{
		k8sClientIndexAIGatewayRouteToAttachedGateway: fmt.Sprintf("%s.%s", req.Name, req.Namespace),
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// 没有aigatewayroute，返回
	if len(routes.Items) == 0 {
		// This means that the gateway is not attached to any AIGatewayRoute.
		c.logger.Info("No AIGatewayRoute attached to the Gateway", "namespace", gw.Namespace, "name", gw.Name)
		return ctrl.Result{}, nil
	}
	// 设置extensionPolicy
	if err := c.ensureExtensionPolicy(ctx, &gw); err != nil {
		return ctrl.Result{}, err
	}

	// We need to create the filter config in Envoy Gateway system namespace because the sidecar extproc need
	// to access it.
	if err := c.reconcileFilterConfigSecret(ctx, &gw, routes.Items, gw.Name); err != nil {
		return ctrl.Result{}, err
	}

	// Finally, we need to annotate the pods of the gateway deployment with the new uuid to propagate the filter config Secret update faster.
	// If the pod doesn't have the extproc container, it will roll out the deployment altogether which eventually ends up
	// the mutation hook invoked.
	if err := c.annotateGatewayPods(ctx, &gw, uuid.NewString()); err != nil {
		c.logger.Error(err, "Failed to annotate gateway pods", "namespace", gw.Namespace, "name", gw.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

const sideCarExtProcBackendName = "envoy-ai-gateway-extproc-backend"

// ensureExtensionPolicy creates or updates the extension policy for the external process running as a sidecar.
func (c *GatewayController) ensureExtensionPolicy(ctx context.Context, gw *gwapiv1.Gateway) (err error) {
	// Ensure that the backend that makes Envoy talk to the UDS exists.
	var backend egv1a1.Backend
	if err = c.client.Get(ctx, client.ObjectKey{Name: sideCarExtProcBackendName, Namespace: gw.Namespace}, &backend); err != nil {
		// 如果不存在这个backend,创建它
		if apierrors.IsNotFound(err) {
			backend = egv1a1.Backend{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sideCarExtProcBackendName,
					Namespace: gw.Namespace,
				},
				Spec: egv1a1.BackendSpec{
					// 指明使用unixsocket模式
					// 路径为extPro的路径
					// extPro会与envoy proxy运行在同一pod中
					// 都挂载这个路径, 以实现共享
					Endpoints: []egv1a1.BackendEndpoint{{Unix: &egv1a1.UnixSocket{Path: c.udsPath}}},
				},
			}
			if err = c.client.Create(ctx, &backend); err != nil {
				return fmt.Errorf("failed to create backend: %w", err)
			}
		} else {
			return fmt.Errorf("failed to get backend: %w", err)
		}
	}

	perGatewayEEPName := fmt.Sprintf("ai-eg-eep-%s", gw.Name)
	var existingPolicy egv1a1.EnvoyExtensionPolicy
	if err = c.client.Get(ctx, client.ObjectKey{Name: perGatewayEEPName, Namespace: gw.Namespace}, &existingPolicy); err == nil {
		return
	} else if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to get extension policy: %w", err)
	}

	// 更新EnvoyExtensionPolicy
	// 申明extPro是envoy-ai-gateway-extproc-backend这个backend(上面创建的)
	extPolicy := &egv1a1.EnvoyExtensionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: perGatewayEEPName, Namespace: gw.Namespace},
		Spec: egv1a1.EnvoyExtensionPolicySpec{
			PolicyTargetReferences: egv1a1.PolicyTargetReferences{TargetRefs: []gwapiv1a2.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gwapiv1a2.LocalPolicyTargetReference{
						Kind:  "Gateway",
						Group: "gateway.networking.k8s.io",
						Name:  gwapiv1.ObjectName(gw.Name),
					},
				},
			}},
			ExtProc: []egv1a1.ExtProc{{
				ProcessingMode: &egv1a1.ExtProcProcessingMode{
					AllowModeOverride: true, // Streaming completely overrides the buffered mode.
					Request:           &egv1a1.ProcessingModeOptions{Body: ptr.To(egv1a1.BufferedExtProcBodyProcessingMode)},
					Response:          &egv1a1.ProcessingModeOptions{Body: ptr.To(egv1a1.BufferedExtProcBodyProcessingMode)},
				},
				Metadata: &egv1a1.ExtProcMetadata{WritableNamespaces: []string{aigv1a1.AIGatewayFilterMetadataNamespace}},
				BackendCluster: egv1a1.BackendCluster{
					BackendRefs: []egv1a1.BackendRef{{
						BackendObjectReference: gwapiv1.BackendObjectReference{
							Name:      gwapiv1.ObjectName(sideCarExtProcBackendName),
							Kind:      ptr.To(gwapiv1.Kind("Backend")),
							Group:     ptr.To(gwapiv1.Group("gateway.envoyproxy.io")),
							Namespace: ptr.To(gwapiv1.Namespace(gw.Namespace)),
						},
					}},
					BackendSettings: &egv1a1.ClusterSettings{
						Connection: &egv1a1.BackendConnection{
							// Default is 32768 bytes == 32 KiB which seems small:
							// https://github.com/envoyproxy/gateway/blob/932b8b55fa562ae917da19b497a4370733478f1/internal/xds/translator/cluster.go#L49
							//
							// So, we set it to 50MBi.
							//
							// Note that currently ExtProc cluster is also defined in the extension server,
							// so ensure that the same value is used there.
							BufferLimit: ptr.To(resource.MustParse("50Mi")),
						},
					},
				},
			}},
		},
	}
	if err = c.client.Create(ctx, extPolicy); err != nil {
		err = fmt.Errorf("failed to create extension policy: %w", err)
	}
	return
}

// schemaToFilterAPI converts an aigv1a1.VersionedAPISchema to filterapi.VersionedAPISchema.
// 将schema转为filterapi.VersionAPISchema
func schemaToFilterAPI(schema aigv1a1.VersionedAPISchema) filterapi.VersionedAPISchema {
	ret := filterapi.VersionedAPISchema{}
	ret.Name = filterapi.APISchemaName(schema.Name)
	// 目前就对schema为OpenAI的做了下额外处理
	if schema.Name == aigv1a1.APISchemaOpenAI {
		// When the schema is OpenAI, we default to the v1 version if not specified or nil.
		ret.Version = cmp.Or(ptr.Deref(schema.Version, "v1"), "v1")
	} else {
		ret.Version = ptr.Deref(schema.Version, "")
	}
	return ret
}

// reconcileFilterConfigSecret updates the filter config secret for the external processor.
// 配置filter-config.yaml，保存到一个secret中
func (c *GatewayController) reconcileFilterConfigSecret(ctx context.Context, gw *gwapiv1.Gateway, aiGatewayRoutes []aigv1a1.AIGatewayRoute, uuid string) error {
	// Precondition: aiGatewayRoutes is not empty as we early return if it is empty.
	// basic.yaml里面只有一个name:  OpenAI, 没有version
	input := aiGatewayRoutes[0].Spec.APISchema

	// uuid为gateway的name
	ec := &filterapi.Config{UUID: uuid}
	// schema = name + version
	// OpenAI默认版本为v1，所以转换后为 OpenAI + v1
	ec.Schema = schemaToFilterAPI(input)
	// x-ai-eg-model, 所有模型的key都为这个
	ec.ModelNameHeaderKey = aigv1a1.AIModelHeaderKey
	// x-ai-eg-selected-route TODO
	ec.SelectedRouteHeaderKey = selectedRouteHeaderKey
	var err error
	// 记录llm，用于去重
	llmCosts := map[string]struct{}{}
	for i := range aiGatewayRoutes {
		// 拿到第一个aiGatewayRoute
		aiGatewayRoute := &aiGatewayRoutes[i]
		spec := aiGatewayRoute.Spec
		// 一个aiGatewayRoute有多个rules，一个rule通过匹配一定的条件，将请求发往特定的backend
		for i := range spec.Rules {
			// 拿到第一个rule
			rule := &spec.Rules[i]
			// BackendRefs对应一个aiServiceBackend，一个aiServiceBackend对应Backend(同名)，在basic.yaml里只有一个名字，其实还有权重, 优先级等
			// 需要将rule的backendRefs转换为[]filterapi.Backend
			backends := make([]filterapi.Backend, len(rule.BackendRefs))
			for j := range rule.BackendRefs {
				// 取到BackendRefs中的第j个backend
				backendRef := &rule.BackendRefs[j]
				// 为第j个filterapi.Backend赋值
				b := &backends[j]
				// 名字和rule里的backend名字一样，命名空间也一样
				b.Name = fmt.Sprintf("%s.%s", backendRef.Name, aiGatewayRoute.Namespace)
				// 是否使用别名，如果有的话
				b.ModelNameOverride = backendRef.ModelNameOverride
				var backendObj *aigv1a1.AIServiceBackend
				// 从k8s中获取AIServiceBackend实例
				backendObj, err = c.backend(ctx, aiGatewayRoute.Namespace, backendRef.Name)
				if err != nil {
					return fmt.Errorf("failed to get AIServiceBackend %s: %w", b.Name, err)
				}
				// 将AIServiceBackend的APISchema也转化为filter的schema
				b.Schema = schemaToFilterAPI(backendObj.Spec.APISchema)
				// 如果该aiServiceBackend引用了一个BackendSecurityPolicyRef
				if bspRef := backendObj.Spec.BackendSecurityPolicyRef; bspRef != nil {
					// 目前主要是做认证，目前适配了以下认证
					/*
						// BackendAuth corresponds partially to BackendSecurityPolicy in api/v1alpha1/api.go.
						type BackendAuth struct {
							// APIKey is a location of the api key secret file.
							APIKey *APIKeyAuth `json:"apiKey,omitempty"`
							// AWSAuth specifies the location of the AWS credential file and region.
							AWSAuth *AWSAuth `json:"aws,omitempty"`
							// AzureAuth specifies the location of Azure access token file.
							AzureAuth *AzureAuth `json:"azure,omitempty"`
							// GCPAuth specifies the location of GCP credential file.
							GCPAuth *GCPAuth `json:"gcp,omitempty"`
						}
					*/
					b.Auth, err = c.bspToFilterAPIBackendAuth(ctx, aiGatewayRoute.Namespace, string(bspRef.Name))
					if err != nil {
						return fmt.Errorf("failed to create backend auth: %w", err)
					}
				}
			}
			// 基于filter.backends构建configRule
			configRule := filterapi.RouteRule{Backends: backends}
			// aigatewayroute的名字 + rule + 当前rule的索引来标识一个configRule
			// fmt.Sprintf("%s-rule-%d", aiGatewayRoute.Name, ruleIndex)
			configRule.Name = routeName(aiGatewayRoute, i)
			// 将rule里面配置的matches转换为configRule的headers
			/*
			  rules:
			    - matches:
			        - headers:
			            - type: Exact
			              name: x-ai-eg-model
			              value: gpt-4o-mini
			*/
			configRule.Headers = make([]filterapi.HeaderMatch, len(rule.Matches))
			for j, match := range rule.Matches {
				configRule.Headers[j].Name = match.Headers[0].Name
				configRule.Headers[j].Value = match.Headers[0].Value
			}
			// 如果rule.ModelsOwnedBy有设置,则使用设置的owner, 否则使用Envoy AI Gateway
			configRule.ModelsOwnedBy = ptr.Deref(rule.ModelsOwnedBy, defaultOwnedBy)
			// Convert to UTC time in force to avoid timezone issues.
			// 设置创建时间
			configRule.ModelsCreatedAt = ptr.Deref[metav1.Time](rule.ModelsCreatedAt, aiGatewayRoute.CreationTimestamp).Time.UTC()
			// 将configRule添加到ec.Rules中 即filterapi.Config中
			ec.Rules = append(ec.Rules, configRule)

			// 如果在aiGatewayRoute中还设置了LLMRequestCosts
			for _, cost := range aiGatewayRoute.Spec.LLMRequestCosts {
				// 新建一个filterapi.LLMRequestCost
				/*
				  llmRequestCosts:
				    - metadataKey: llm_input_token
				      type: InputToken
				    - metadataKey: llm_cel_calculated_token
				      type: CEL
				      cel: "input_tokens == uint(3) ? 100000000 : 0"
				*/
				fc := filterapi.LLMRequestCost{MetadataKey: cost.MetadataKey}
				// 不能重名
				_, ok := llmCosts[cost.MetadataKey]
				if ok {
					c.logger.Info("LLMRequestCost with the same metadata key already exists, skipping",
						"metadataKey", cost.MetadataKey, "route", aiGatewayRoute.Name)
					continue
				}
				// 支持以下类型
				switch cost.Type {
				case aigv1a1.LLMRequestCostTypeInputToken:
					fc.Type = filterapi.LLMRequestCostTypeInputToken
				case aigv1a1.LLMRequestCostTypeOutputToken:
					fc.Type = filterapi.LLMRequestCostTypeOutputToken
				case aigv1a1.LLMRequestCostTypeTotalToken:
					fc.Type = filterapi.LLMRequestCostTypeTotalToken
				case aigv1a1.LLMRequestCostTypeCEL:
					fc.Type = filterapi.LLMRequestCostTypeCEL
					expr := *cost.CEL
					// Sanity check the CEL expression.
					_, err = llmcostcel.NewProgram(expr)
					if err != nil {
						return fmt.Errorf("invalid CEL expression: %w", err)
					}
					fc.CEL = expr
				default:
					return fmt.Errorf("unknown request cost type: %s", cost.Type)
				}
				// 将filterapi.LLMRequestCost添加到ec.LLMRequestCosts中
				ec.LLMRequestCosts = append(ec.LLMRequestCosts, fc)
				llmCosts[cost.MetadataKey] = struct{}{}
			}
		}
	}

	// io.envoy.ai_gateway
	ec.MetadataNamespace = aigv1a1.AIGatewayFilterMetadataNamespace

	marshaled, err := yaml.Marshal(ec)
	if err != nil {
		return fmt.Errorf("failed to marshal extproc config: %w", err)
	}

	// gateway的名字和命名空间
	// fmt.Sprintf("%s-%s", gwName, gwNamespace)
	name := FilterConfigSecretPerGatewayName(gw.Name, gw.Namespace)
	// We need to create the filter config in Envoy Gateway system namespace because the sidecar extproc need
	// to access it.
	// filter-config.yaml
	data := map[string]string{FilterConfigKeyInSecret: string(marshaled)}
	// 保存到一个secret里面，名字为filter-config.yaml
	secret, err := c.kube.CoreV1().Secrets(c.envoyGatewayNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// 不存在,则创建
		if apierrors.IsNotFound(err) {
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.envoyGatewayNamespace},
				StringData: data,
			}
			if _, err = c.kube.CoreV1().Secrets(c.envoyGatewayNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("failed to create secret %s: %w", name, err)
			}
			return nil
		}
		return fmt.Errorf("failed to get secret %s: %w", name, err)
	}

	// 存在则更新
	secret.StringData = data
	if _, err := c.kube.CoreV1().Secrets(c.envoyGatewayNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update secret %s: %w", secret.Name, err)
	}
	return nil
}

func (c *GatewayController) bspToFilterAPIBackendAuth(ctx context.Context, namespace, bspName string) (*filterapi.BackendAuth, error) {
	backendSecurityPolicy, err := c.backendSecurityPolicy(ctx, namespace, bspName)
	if err != nil {
		return nil, fmt.Errorf("failed to get BackendSecurityPolicy %s: %w", bspName, err)
	}
	switch backendSecurityPolicy.Spec.Type {
	case aigv1a1.BackendSecurityPolicyTypeAPIKey:
		secretName := string(backendSecurityPolicy.Spec.APIKey.SecretRef.Name)
		apiKey, err := c.getSecretData(ctx, namespace, secretName, apiKeyInSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
		}
		return &filterapi.BackendAuth{APIKey: &filterapi.APIKeyAuth{Key: apiKey}}, nil
	case aigv1a1.BackendSecurityPolicyTypeAWSCredentials:
		if backendSecurityPolicy.Spec.AWSCredentials == nil {
			return nil, fmt.Errorf("AWSCredentials type selected but not defined %s", backendSecurityPolicy.Name)
		}
		var secretName string
		if awsCred := backendSecurityPolicy.Spec.AWSCredentials; awsCred.CredentialsFile != nil {
			secretName = string(awsCred.CredentialsFile.SecretRef.Name)
		} else {
			secretName = rotators.GetBSPSecretName(backendSecurityPolicy.Name)
		}
		credentialsLiteral, err := c.getSecretData(ctx, namespace, secretName, rotators.AwsCredentialsKey)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
		}
		if awsCred := backendSecurityPolicy.Spec.AWSCredentials; awsCred.CredentialsFile != nil || awsCred.OIDCExchangeToken != nil {
			return &filterapi.BackendAuth{
				AWSAuth: &filterapi.AWSAuth{
					CredentialFileLiteral: credentialsLiteral,
					Region:                backendSecurityPolicy.Spec.AWSCredentials.Region,
				},
			}, nil
		}
		return nil, nil
	case aigv1a1.BackendSecurityPolicyTypeAzureCredentials:
		if backendSecurityPolicy.Spec.AzureCredentials == nil {
			return nil, fmt.Errorf("AzureCredentials type selected but not defined %s", backendSecurityPolicy.Name)
		}
		secretName := rotators.GetBSPSecretName(backendSecurityPolicy.Name)
		azureAccessToken, err := c.getSecretData(ctx, namespace, secretName, rotators.AzureAccessTokenKey)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
		}
		return &filterapi.BackendAuth{
			AzureAuth: &filterapi.AzureAuth{AccessToken: azureAccessToken},
		}, nil
	case aigv1a1.BackendSecurityPolicyTypeGCPCredentials:
		gcpCreds := backendSecurityPolicy.Spec.GCPCredentials
		if gcpCreds == nil {
			return nil, fmt.Errorf("GCP credentials type selected but not defined %s", backendSecurityPolicy.Name)
		}
		secretName := rotators.GetBSPSecretName(backendSecurityPolicy.Name)
		gcpAccessToken, err := c.getSecretData(ctx, namespace, secretName, rotators.GCPAccessTokenKey)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
		}
		return &filterapi.BackendAuth{
			GCPAuth: &filterapi.GCPAuth{
				AccessToken: gcpAccessToken,
				Region:      gcpCreds.Region,
				ProjectName: gcpCreds.ProjectName,
			},
		}, nil
	default:
		return nil, fmt.Errorf("invalid backend security type %s for policy %s", backendSecurityPolicy.Spec.Type,
			backendSecurityPolicy.Name)
	}
}

func (c *GatewayController) getSecretData(ctx context.Context, namespace, name, dataKey string) (string, error) {
	secret, err := c.kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s: %w", name, err)
	}
	if secret.Data != nil {
		if value, ok := secret.Data[dataKey]; ok {
			return string(value), nil
		}
	}
	if secret.StringData != nil {
		if value, ok := secret.StringData[dataKey]; ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("secret %s does not contain key %s", name, dataKey)
}

func (c *GatewayController) backend(ctx context.Context, namespace, name string) (*aigv1a1.AIServiceBackend, error) {
	backend := &aigv1a1.AIServiceBackend{}
	return backend, c.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, backend)
}

func (c *GatewayController) backendSecurityPolicy(ctx context.Context, namespace, name string) (*aigv1a1.BackendSecurityPolicy, error) {
	backendSecurityPolicy := &aigv1a1.BackendSecurityPolicy{}
	return backendSecurityPolicy, c.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, backendSecurityPolicy)
}

// annotateGatewayPods annotates the pods of GW with the new uuid to propagate the filter config Secret update faster.
// If the pod doesn't have the extproc container, it will roll out the deployment altogether, which eventually ends up
// the mutation hook invoked.
// 为网关节点（GW）的 Pod 标注新的 UUID，以便更快地除非filter config的Secret更新。
// 如果 Pod 没有extproc容器，直接将deployment重启，最终会调用mutating webhook。
// See https://neonmirrors.net/post/2022-12/reducing-pod-volume-update-times/ for explanation.
func (c *GatewayController) annotateGatewayPods(ctx context.Context, gw *gwapiv1.Gateway, uuid string) error {
	pods, err := c.kube.CoreV1().Pods(c.envoyGatewayNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			// gateway.envoyproxy.io/owning-gateway-name
			// gateway.envoyproxy.io/owning-gateway-namespace
			// 有一个webhook会自动注入这些label
			egOwningGatewayNameLabel, gw.Name, egOwningGatewayNamespaceLabel, gw.Namespace),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	rollout := true
	for _, pod := range pods.Items {
		// Get the pod spec and check if it has the extproc container.
		podSpec := pod.Spec
		for i := range podSpec.Containers {
			// If there's an extproc container with the current target image, we don't need to roll out the deployment.
			// ai-gateway-extproc，有这个pod，则不需要重启
			if podSpec.Containers[i].Name == extProcContainerName && podSpec.Containers[i].Image == c.extProcImage {
				rollout = false
				break
			}
		}

		c.logger.Info("annotating pod", "namespace", pod.Namespace, "name", pod.Name)
		// 在annotations里面设置一个uuid
		_, err = c.kube.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType,
			[]byte(fmt.Sprintf(
				`{"metadata":{"annotations":{"%s":"%s"}}}`, aigatewayUUIDAnnotationKey, uuid),
			), metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("failed to patch pod %s: %w", pod.Name, err)
		}
	}

	if rollout {
		deps, err := c.kube.AppsV1().Deployments(c.envoyGatewayNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
				egOwningGatewayNameLabel, gw.Name, egOwningGatewayNamespaceLabel, gw.Namespace),
		})
		if err != nil {
			return fmt.Errorf("failed to list deployments: %w", err)
		}
		// 给deploy打patch
		for _, dep := range deps.Items {
			c.logger.Info("rolling out deployment", "namespace", dep.Namespace, "name", dep.Name)
			_, err = c.kube.AppsV1().Deployments(dep.Namespace).Patch(ctx, dep.Name, types.MergePatchType,
				[]byte(fmt.Sprintf(
					`{"spec":{"template":{"metadata":{"annotations":{"%s":"%s"}}}}}`, aigatewayUUIDAnnotationKey, uuid),
				), metav1.PatchOptions{})
			if err != nil {
				return fmt.Errorf("failed to patch deployment %s: %w", dep.Name, err)
			}
		}
	}
	return nil
}
