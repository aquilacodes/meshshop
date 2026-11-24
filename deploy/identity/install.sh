#!/usr/bin/env bash
# Deploys Keycloak (namespace identity) with the meshshop realm. Idempotent.
# Generates the admin and demo-user passwords on first run and keeps them in
# Secrets; nothing secret lives in the repo.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NS=identity

kubectl apply -f "$DIR/namespace.yaml"
rand() { head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }
secret() { # name key=value...
  kubectl -n "$NS" get secret "$1" >/dev/null 2>&1 && return 0
  local args=(); for kv in "${@:2}"; do args+=(--from-literal="$kv"); done
  kubectl -n "$NS" create secret generic "$1" "${args[@]}" >/dev/null
  echo "  created secret $NS/$1"
}
secret keycloak-admin username=admin "password=$(rand)"
secret meshshop-demo-users "alice=$(rand)" "bob=$(rand)"

kubectl apply -k "$DIR"
kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s
