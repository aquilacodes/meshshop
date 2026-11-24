#!/usr/bin/env bash
# Shows that Istio AuthorizationPolicy enforces bff -> business -> domain.
#
# The service images are FROM scratch (no curl), and curl from the istio-proxy
# container bypasses the sidecar (UID 1337 is excluded from interception), so
# we start a short-lived probe pod per source identity instead. Each probe pod
# runs under the real service's ServiceAccount, so its sidecar presents the same
# SPIFFE identity (spiffe://cluster.local/ns/meshshop/sa/<name>) as the service.
set -uo pipefail
CTX="${1:-meshshop}"
NS="${2:-meshshop}"
IMAGE="${PROBE_IMAGE:-registry.istio.io/release/proxyv2:1.30.8@sha256:a40d8ac073a342b07a748e6c81675636b298cd3e9bd6a7eb14036187b052d350}" # has curl
SOURCES=(web-bff shopping catalog inventory order)
k() { kubectl --context "$CTX" -n "$NS" "$@"; }

cleanup() { k delete pod -l meshshop.io/probe=true --ignore-not-found --wait=false >/dev/null; }
trap cleanup EXIT

for sa in "${SOURCES[@]}"; do
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: probe-as-${sa}
  labels: { meshshop.io/probe: "true" }
spec:
  serviceAccountName: ${sa}
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 0
  securityContext:   # the namespace enforces the "restricted" Pod Security Standard
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: probe
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sleep", "3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: [ALL] }
EOF
done
k wait --for=condition=Ready pod -l meshshop.io/probe=true --timeout=120s >/dev/null || exit 1

pass=0 fail=0
probe() { # from to path expected
  local code
  code=$(k exec "probe-as-$1" -c probe -- curl -s -o /dev/null -w '%{http_code}' "http://$2:8080$3")
  if [[ "$code" == "$4" ]]; then
    echo "PASS  as $1 -> $2$3 = $code"; pass=$((pass + 1))
  else
    echo "FAIL  as $1 -> $2$3 = $code (want $4)"; fail=$((fail + 1))
  fi
}

echo "Allowed (adjacent layers):"
probe web-bff  shopping  /v1/products                 200
probe shopping catalog   /v1/products                 200
probe shopping inventory "/v1/stock?productIds=p-100" 200
probe shopping order     "/v1/orders?customerId=x"    200
echo "Denied (layer-skipping, sideways, upward):"
probe web-bff   catalog   /v1/products                 403
probe web-bff   order     "/v1/orders?customerId=x"    403
probe catalog   inventory "/v1/stock?productIds=p-100" 403
probe order     shopping  /v1/products                 403
probe inventory web-bff   /api/home                    403

echo; echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
