#!/bin/sh
set -eu
# Isolated kubeconfig; never uses or mutates the user's current context.
cd "$(dirname "$0")/.."
cluster="wuko-kube-test-$$"
config=$(mktemp)
cleanup() { kind delete cluster --name "$cluster"; rm -f "$config"; }
trap cleanup EXIT INT TERM
kind create cluster --name "$cluster" --image "${KIND_NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}" --kubeconfig "$config" --wait 120s
docker build -t wuko-worker:dev .
docker build -t wuko-kube-sink:dev integration/sink
docker build -t wuko-k6:dev examples/k6
kind load docker-image --name "$cluster" wuko-worker:dev wuko-kube-sink:dev wuko-k6:dev
# Generate local plugin bundle for the unmodified CLI install/crash test.
go run ./tools/release -version 0.1.0
WUKO_KUBE_K6_IMAGE=wuko-k6:dev WUKO_KUBE_INTEGRATION=1 KUBECONFIG="$config" go test -count=1 -timeout=10m -run TestKubernetesIntegration -v .
