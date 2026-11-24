#!/usr/bin/env bash
# Installs Istio (base CRDs, CNI node agent, istiod, ingress gateway) with Helm,
# plus the gateway's TLS certificate. Idempotent.
set -euo pipefail
ISTIO_VERSION="${ISTIO_VERSION:-1.30.8}"
DIR="$(cd "$(dirname "$0")" && pwd)"
CERTS="$DIR/../../.certs"

helm repo add istio https://istio-release.storage.googleapis.com/charts >/dev/null 2>&1 || true
helm repo update istio >/dev/null

helm upgrade --install istio-base istio/base -n istio-system --create-namespace \
  --version "$ISTIO_VERSION" --set defaultRevision=default --wait

# The CNI node agent sets up traffic redirection when a pod's network is created,
# so injected pods need no privileged istio-init container (NET_ADMIN/NET_RAW)
# and can run under the "restricted" Pod Security Standard.
helm upgrade --install istio-cni istio/cni -n istio-system \
  --version "$ISTIO_VERSION" -f "$DIR/cni-values.yaml" --wait

helm upgrade --install istiod istio/istiod -n istio-system \
  --version "$ISTIO_VERSION" -f "$DIR/istiod-values.yaml" --wait

kubectl apply -f "$DIR/namespaces.yaml"

# HTTPS certificate for the edge (scripts/certs.sh issues it from the local CA).
kubectl -n istio-ingress create secret tls meshshop-gateway-tls \
  --cert="$CERTS/gateway.crt" --key="$CERTS/gateway.key" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install istio-ingressgateway istio/gateway -n istio-ingress \
  --version "$ISTIO_VERSION" -f "$DIR/gateway-values.yaml" --wait

kubectl apply -f "$DIR/edge"

echo "Istio $ISTIO_VERSION installed."
