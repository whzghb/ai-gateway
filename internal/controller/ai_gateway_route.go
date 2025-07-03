// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"fmt"
	"strings"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/filterapi"
)

const (
	managedByLabel             = "app.kubernetes.io/managed-by"
	selectedRouteHeaderKey     = "x-ai-eg-selected-route"
	hostRewriteHTTPFilterName  = "ai-eg-host-rewrite"
	aigatewayUUIDAnnotationKey = "aigateway.envoyproxy.io/uuid"
	// We use this annotation to ensure that Envoy Gateway reconciles the HTTPRoute when the backend refs change.
	// This will result in metadata being added to the underling Envoy route
	// @see https://gateway.envoyproxy.io/contributions/design/metadata/
	httpRouteBackendRefPriorityAnnotationKey = "gateway.envoyproxy.io/backend-ref-priority"
	// gatewayapi里的约定
	egOwningGatewayNameLabel      = "gateway.envoyproxy.io/owning-gateway-name"
	egOwningGatewayNamespaceLabel = "gateway.envoyproxy.io/owning-gateway-namespace"
	// apiKeyInSecret is the key to store OpenAI API key.
	apiKeyInSecret = "apiKey"
)

// AIGatewayRouteController implements [reconcile.TypedReconciler].
//
// This handles the AIGatewayRoute resource and creates the necessary resources for the external process.
//
// Exported for testing purposes.
type AIGatewayRouteController struct {
	client client.Client
	kube   kubernetes.Interface
	logger logr.Logger
	// gatewayEventChan is a channel to send events to the gateway controller.
	gatewayEventChan chan event.GenericEvent
}

// NewAIGatewayRouteController creates a new reconcile.TypedReconciler[reconcile.Request] for the AIGatewayRoute resource.
func NewAIGatewayRouteController(
	client client.Client, kube kubernetes.Interface, logger logr.Logger,
	gatewayEventChan chan event.GenericEvent,
) *AIGatewayRouteController {
	return &AIGatewayRouteController{
		client:           client,
		kube:             kube,
		logger:           logger,
		gatewayEventChan: gatewayEventChan,
	}
}

// Reconcile implements [reconcile.TypedReconciler].
func (c *AIGatewayRouteController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	c.logger.Info("Reconciling AIGatewayRoute", "namespace", req.Namespace, "name", req.Name)

	var aiGatewayRoute aigv1a1.AIGatewayRoute
	if err := c.client.Get(ctx, req.NamespacedName, &aiGatewayRoute); err != nil {
		if client.IgnoreNotFound(err) == nil {
			c.logger.Info("Deleting AIGatewayRoute",
				"namespace", req.Namespace, "name", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 同步aiGatewayRoute
	if err := c.syncAIGatewayRoute(ctx, &aiGatewayRoute); err != nil {
		c.logger.Error(err, "failed to sync AIGatewayRoute")
		// 不成功更新为NotAccepted状态
		c.updateAIGatewayRouteStatus(ctx, &aiGatewayRoute, aigv1a1.ConditionTypeNotAccepted, err.Error())
		return ctrl.Result{}, err
	}
	// 更新为成功
	c.updateAIGatewayRouteStatus(ctx, &aiGatewayRoute, aigv1a1.ConditionTypeAccepted, "AI Gateway Route reconciled successfully")
	return reconcile.Result{}, nil
}

func FilterConfigSecretPerGatewayName(gwName, gwNamespace string) string {
	return fmt.Sprintf("%s-%s", gwName, gwNamespace)
}

// syncAIGatewayRoute is the main logic for reconciling the AIGatewayRoute resource.
// This is decoupled from the Reconcile method to centralize the error handling and status updates.
func (c *AIGatewayRouteController) syncAIGatewayRoute(ctx context.Context, aiGatewayRoute *aigv1a1.AIGatewayRoute) error {
	// Check if the HTTPRouteFilter exists in the namespace.
	// 获取HTTPRouteFilter资源
	/*
		apiVersion: gateway.envoyproxy.io/v1alpha1
		kind: HTTPRouteFilter
		metadata:
		  name: ai-eg-host-rewrite
		  namespace: default
		spec:
		  urlRewrite:
		    hostname:
		      type: Backend
	*/
	var httpRouteFilter egv1a1.HTTPRouteFilter
	err := c.client.Get(ctx,
		client.ObjectKey{Name: hostRewriteHTTPFilterName, Namespace: aiGatewayRoute.Namespace}, &httpRouteFilter)
	if apierrors.IsNotFound(err) {
		// 如果不存在，创建一个，一个命名空间有且只存在一个，ai-eg-host-rewrite
		httpRouteFilter = egv1a1.HTTPRouteFilter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hostRewriteHTTPFilterName,
				Namespace: aiGatewayRoute.Namespace,
			},
			// hostname.Type也是固定的: Backend
			Spec: egv1a1.HTTPRouteFilterSpec{
				URLRewrite: &egv1a1.HTTPURLRewriteFilter{
					Hostname: &egv1a1.HTTPHostnameModifier{
						Type: egv1a1.BackendHTTPHostnameModifier,
					},
				},
			},
		}
		if err = c.client.Create(ctx, &httpRouteFilter); err != nil {
			return fmt.Errorf("failed to create HTTPRouteFilter: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get HTTPRouteFilter: %w", err)
	}

	// Check if the HTTPRoute exists.
	c.logger.Info("syncing AIGatewayRoute", "namespace", aiGatewayRoute.Namespace, "name", aiGatewayRoute.Name)
	var httpRoute gwapiv1.HTTPRoute
	err = c.client.Get(ctx, client.ObjectKey{Name: aiGatewayRoute.Name, Namespace: aiGatewayRoute.Namespace}, &httpRoute)
	existingRoute := err == nil
	if apierrors.IsNotFound(err) {
		// This means that this AIGatewayRoute is a new one.
		// 不存在，新创建一个httproute，一个AIGatewayRoute对应一个HTTPRoute
		httpRoute = gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      aiGatewayRoute.Name,
				Namespace: aiGatewayRoute.Namespace,
			},
			Spec: gwapiv1.HTTPRouteSpec{},
		}
		// 设置controllerrefrence，可以级联删除和触发调谐
		if err = ctrlutil.SetControllerReference(aiGatewayRoute, &httpRoute, c.client.Scheme()); err != nil {
			panic(fmt.Errorf("BUG: failed to set controller reference for HTTPRoute: %w", err))
		}
	} else if err != nil {
		return fmt.Errorf("failed to get HTTPRoute: %w", err)
	}

	// Update the HTTPRoute with the new AIGatewayRoute.
	// 使用AIGatewayRoute更新或创建HTTPRoute
	if err = c.newHTTPRoute(ctx, &httpRoute, aiGatewayRoute); err != nil {
		return fmt.Errorf("failed to construct a new HTTPRoute: %w", err)
	}

	// 如果httproute已经存在，更新
	if existingRoute {
		c.logger.Info("updating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
		if err = c.client.Update(ctx, &httpRoute); err != nil {
			return fmt.Errorf("failed to update HTTPRoute: %w", err)
		}
	} else {
		// 创建
		c.logger.Info("creating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
		if err = c.client.Create(ctx, &httpRoute); err != nil {
			return fmt.Errorf("failed to create HTTPRoute: %w", err)
		}
	}

	err = c.syncGateways(ctx, aiGatewayRoute)
	if err != nil {
		return fmt.Errorf("failed to sync gw pods: %w", err)
	}
	return nil
}

func routeName(aiGatewayRoute *aigv1a1.AIGatewayRoute, ruleIndex int) filterapi.RouteRuleName {
	return filterapi.RouteRuleName(fmt.Sprintf("%s-rule-%d", aiGatewayRoute.Name, ruleIndex))
}

// newHTTPRoute updates the HTTPRoute with the new AIGatewayRoute.
func (c *AIGatewayRouteController) newHTTPRoute(ctx context.Context, dst *gwapiv1.HTTPRoute, aiGatewayRoute *aigv1a1.AIGatewayRoute) error {
	// 初始化rewriteFilters，是一个列表格式，目前只有一个，所有httproute里面的rules都共用这个rewritefilters
	rewriteFilters := []gwapiv1.HTTPRouteFilter{{
		// ExtensionRef
		Type: gwapiv1.HTTPRouteFilterExtensionRef,
		ExtensionRef: &gwapiv1.LocalObjectReference{
			Group: "gateway.envoyproxy.io",
			Kind:  "HTTPRouteFilter",
			// ai-eg-host-rewrite
			Name: hostRewriteHTTPFilterName,
		},
	}}
	// HTTPRouteRule，如下格式
	/*
	  rules:
	  - backendRefs:
	    - group: gateway.envoyproxy.io
	      kind: Backend
	      name: envoy-ai-gateway-basic-openai
	      weight: 1
	    filters:
	    - extensionRef:
	        group: gateway.envoyproxy.io
	        kind: HTTPRouteFilter
	        name: ai-eg-host-rewrite
	      type: ExtensionRef
	    matches:
	    - headers:
	      - name: x-ai-eg-selected-route
	        type: Exact
	        value: envoy-ai-gateway-basic-rule-0
	      path:
	        type: PathPrefix
	        value: /
	    timeouts:
	      request: 3s
	*/
	var rules []gwapiv1.HTTPRouteRule
	// aiGatewayRoute.Spec.Rules
	/*
		rules:
		    - matches:
		        - headers:
		            - type: Exact
		              name: x-ai-eg-model
		              value: gpt-4o-mini
		      backendRefs:
		        - name: envoy-ai-gateway-basic-openai
	*/
	// 将aiGatewayRoute.Spec.Rules转换为HTTPRouteRule
	for i, rule := range aiGatewayRoute.Spec.Rules {
		// filterapi.RouteRuleName(fmt.Sprintf("%s-rule-%d", aiGatewayRoute.Name, ruleIndex))
		routeName := routeName(aiGatewayRoute, i)
		// 一个rule的backendRef可以是多个，即可以对应多个backend
		var backendRefs []gwapiv1.HTTPBackendRef
		for i := range rule.BackendRefs {
			// 获取第i个backend
			br := &rule.BackendRefs[i]
			// 目标名：backendName + namespace
			dstName := fmt.Sprintf("%s.%s", br.Name, aiGatewayRoute.Namespace)
			// 从k8s中获取完整backend
			backend, err := c.backend(ctx, aiGatewayRoute.Namespace, br.Name)
			if err != nil {
				return fmt.Errorf("AIServiceBackend %s not found", dstName)
			}
			// 转换成了一个HTTPBackendRef
			backendRefs = append(backendRefs,
				gwapiv1.HTTPBackendRef{BackendRef: gwapiv1.BackendRef{
					BackendObjectReference: backend.Spec.BackendRef,
					Weight:                 br.Weight,
				}},
			)
		}
		// 添加进httproute规则列表
		rules = append(rules, gwapiv1.HTTPRouteRule{
			BackendRefs: backendRefs,
			Matches: []gwapiv1.HTTPRouteMatch{
				{Headers: []gwapiv1.HTTPHeaderMatch{{Name: selectedRouteHeaderKey, Value: string(routeName)}}},
			},
			Filters:  rewriteFilters,
			Timeouts: rule.GetTimeoutsOrDefault(),
		})
	}

	// Adds the default route rule with "/" path. This is necessary because Envoy's router selects the backend
	// before entering the filters. So, all requests would result in a 404 if there is no default route. In practice,
	// this default route is not used because our AI Gateway filters is the one who actually calculates the route based
	// on the given Rules. If it doesn't match any backend, 404 will be returned from the AI Gateway filter as an immediate
	// response.
	//
	// In other words, this default route is an implementation detail to make the Envoy router happy and does not affect
	// the actual routing at all.
	// 加一个默认规则，如果全都不匹配，返回unreachable
	if len(rules) > 0 {
		rules = append(rules, gwapiv1.HTTPRouteRule{
			Name:    ptr.To[gwapiv1.SectionName]("unreachable"),
			Matches: []gwapiv1.HTTPRouteMatch{{Path: &gwapiv1.HTTPPathMatch{Value: ptr.To("/")}}},
		})
	}

	dst.Spec.Rules = rules

	if dst.ObjectMeta.Annotations == nil {
		dst.ObjectMeta.Annotations = make(map[string]string)
	}
	// HACK: We need to set an annotation so that Envoy Gateway reconciles the HTTPRoute when the backend refs change.
	// 为了使envoy gateway controller在HTTPRoute.backend发生变化时执行调谐
	dst.ObjectMeta.Annotations[httpRouteBackendRefPriorityAnnotationKey] = buildPriorityAnnotation(aiGatewayRoute.Spec.Rules)

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
	*/
	targetRefs := aiGatewayRoute.Spec.TargetRefs
	egNs := gwapiv1.Namespace(aiGatewayRoute.Namespace)
	// 通常设置为route想要绑定的gateway
	parentRefs := make([]gwapiv1.ParentReference, len(targetRefs))
	for i, egRef := range targetRefs {
		egName := egRef.Name
		var namespace *gwapiv1.Namespace
		if egNs != "" {
			namespace = ptr.To(egNs)
		}
		parentRefs[i] = gwapiv1.ParentReference{
			Name:      egName,
			Namespace: namespace,
		}
	}
	dst.Spec.CommonRouteSpec.ParentRefs = parentRefs
	return nil
}

func (c *AIGatewayRouteController) syncGateways(ctx context.Context, aiGatewayRoute *aigv1a1.AIGatewayRoute) error {
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
	*/
	for _, t := range aiGatewayRoute.Spec.TargetRefs {
		// 触发当前aiGatewayRoute对应的Gateway更新
		var gw gwapiv1.Gateway
		if err := c.client.Get(ctx, client.ObjectKey{Name: string(t.Name), Namespace: aiGatewayRoute.Namespace}, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				c.logger.Info("Gateway not found", "namespace", aiGatewayRoute.Namespace, "name", t.Name)
				continue
			}
			return fmt.Errorf("failed to get Gateway: %w", err)
		}
		c.logger.Info("syncing Gateway", "namespace", gw.Namespace, "name", gw.Name)
		// 传给这个chan，aigateway控制器会监听到
		c.gatewayEventChan <- event.GenericEvent{Object: &gw}
	}
	return nil
}

func (c *AIGatewayRouteController) backend(ctx context.Context, namespace, name string) (*aigv1a1.AIServiceBackend, error) {
	backend := &aigv1a1.AIServiceBackend{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, backend); err != nil {
		return nil, err
	}
	return backend, nil
}

// updateAIGatewayRouteStatus updates the status of the AIGatewayRoute.
func (c *AIGatewayRouteController) updateAIGatewayRouteStatus(ctx context.Context, route *aigv1a1.AIGatewayRoute, conditionType string, message string) {
	route.Status.Conditions = newConditions(conditionType, message)
	if err := c.client.Status().Update(ctx, route); err != nil {
		c.logger.Error(err, "failed to update AIGatewayRoute status")
	}
}

// Build an annotation that contains the priority of each backend ref. This is used to ensure Envoy Gateway reconciles the
// HTTP route when the priorities change.
func buildPriorityAnnotation(rules []aigv1a1.AIGatewayRouteRule) string {
	priorities := make([]string, 0, len(rules))
	for i, rule := range rules {
		for _, br := range rule.BackendRefs {
			var priority uint32
			if br.Priority != nil {
				priority = *br.Priority
			}
			priorities = append(priorities, fmt.Sprintf("%d:%s:%d", i, br.Name, priority))
		}
	}
	return strings.Join(priorities, ",")
}
