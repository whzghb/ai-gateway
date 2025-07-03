#!/bin/bash
 
# kubectl edit deploy -n ai-gateway-controller   set image tag
rm -f out/controller-linux-amd64
rm -f out/extproc-linux-amd64
 
make build.controller
make build.extproc
 
sudo make docker-build TAG=dev
sudo kind load docker-image docker.io/envoyproxy/ai-gateway-controller:dev
sudo kind load docker-image docker.io/envoyproxy/ai-gateway-extproc:dev
 
kubectl rollout restart deploy -n envoy-ai-gateway-system ai-gateway-controller

kubectl delete -f examples/basic/basic.yaml
kubectl apply -f examples/basic/basic.yaml
