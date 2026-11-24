#!/usr/bin/env bash
# Installs the ECK operator and the meshshop observability stack
# (Elasticsearch, Kibana, APM Server, Logstash, Filebeat, OpenTelemetry Collector). Idempotent.
set -euo pipefail
ECK_VERSION="${ECK_VERSION:-3.5.0}"
DIR="$(cd "$(dirname "$0")" && pwd)"
CERTS="$DIR/../../.certs"

helm repo add elastic https://helm.elastic.co >/dev/null 2>&1 || true
helm repo update elastic >/dev/null
helm upgrade --install elastic-operator elastic/eck-operator -n elastic-system --create-namespace \
  --version "$ECK_VERSION" -f "$DIR/eck-operator-values.yaml" --wait

kubectl apply -f "$DIR/stack/namespace.yaml" -f "$DIR/agents/namespace.yaml"

# Mutual TLS between Filebeat and Logstash (certificates from scripts/certs.sh).
tls_secret() { # namespace name cert key
  kubectl -n "$1" create secret generic "$2" --from-file=tls.crt="$CERTS/$3.crt" \
    --from-file=tls.key="$CERTS/$3.key" --from-file=ca.crt="$CERTS/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}
tls_secret observability logstash-beats-tls logstash
tls_secret observability-agents filebeat-client-tls filebeat

# Elasticsearch file-realm users (ECK reads username/password/roles from the
# Secret). Passwords are generated once and never leave the cluster.
user_secret() { # secret username roles
  kubectl -n observability get secret "$1" >/dev/null 2>&1 && return 0
  kubectl -n observability create secret generic "$1" --type=kubernetes.io/basic-auth \
    --from-literal=username="$2" --from-literal=roles="$3" \
    --from-literal=password="$(head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)" >/dev/null
  echo "  created user $2 ($3)"
}
user_secret meshshop-viewer-user meshshop-viewer viewer
user_secret meshshop-setup-user meshshop-setup meshshop_setup

kubectl apply -k "$DIR/stack"
kubectl apply -k "$DIR/agents"

echo "Waiting for the stack (first run pulls ~2 GB of images)..."
wait_for() { # kind name [namespace]
  for _ in $(seq 1 120); do
    h=$(kubectl -n "${3:-observability}" get "$1" "$2" -o jsonpath='{.status.health}' 2>/dev/null || true)
    [[ "$h" == "green" ]] && { echo "  $1/$2 green"; return 0; }
    # One Elasticsearch node can't host replicas; kibana/setup.sh makes new
    # indices replica-free, but data written before it ran can leave it yellow.
    [[ "$1" == elasticsearch && "$h" == "yellow" ]] && { echo "  $1/$2 yellow (single node: unassigned replicas)"; return 0; }
    sleep 5
  done
  echo "  $1/$2 not green after 10m (health=$h)"; return 1
}
wait_for elasticsearch meshshop
wait_for kibana meshshop
wait_for apmserver meshshop
wait_for logstash meshshop
wait_for beat meshshop observability-agents
kubectl -n observability rollout status deploy/otel-collector --timeout=180s

"$DIR/kibana/setup.sh"
