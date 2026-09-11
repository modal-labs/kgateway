# Local rate limit demo (minikube)

Proves out `shareAcrossGateway` (Envoy `local_cluster_rate_limit`) and
descriptor-based local rate limits against a toy echo server.

## Setup

```bash
minikube start --driver=docker --cpus=4 --memory=12g --kubernetes-version=v1.36.1

# build images from this branch and load them into minikube
VERSION=v1.0.0-local make kgateway-docker envoy-wrapper-docker sds-docker package-kgateway-charts
for i in kgateway envoy-wrapper sds; do minikube image load ghcr.io/kgateway-dev/$i:v1.0.0-local; done

make gw-api-crds
VERSION=v1.0.0-local make deploy-kgateway-crd-chart deploy-kgateway-chart

kubectl apply -f hack/local-ratelimit-demo/01-toy-server.yaml \
              -f hack/local-ratelimit-demo/02-gateway.yaml
```

## Phase 1: one bucket shared across all Gateway replicas

```bash
kubectl apply -f hack/local-ratelimit-demo/03-basic-local-ratelimit.yaml   # 10 req / 10s for the route
kubectl -n demo port-forward svc/demo-gw 8080:8080 &
go run ./hack/local-ratelimit-demo/client -url http://127.0.0.1:8080/hello -n 15 -v
```

Expect 10x `200` then `429`. Scale the gateway to 2 replicas and port-forward each
pod individually: each replica now admits 5 (the bucket is divided across the
local cluster).

```bash
kubectl -n demo scale deploy/demo-gw --replicas=2
```

## Inspecting Envoy

```bash
POD=$(kubectl -n demo get pod -l gateway.networking.k8s.io/gateway-name=demo-gw -o name | head -1)
# local cluster membership (should list every gateway pod IP on :19000)
kubectl -n demo exec $POD -- wget -qO- localhost:19000/clusters | grep demo-gw.demo
# rate limit stats
kubectl -n demo exec $POD -- wget -qO- 'localhost:19000/stats?filter=local_rate_limit'
# rendered filter config
kubectl -n demo exec $POD -- wget -qO- localhost:19000/config_dump | grep -A5 local_cluster_rate_limit
```
