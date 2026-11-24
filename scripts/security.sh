#!/usr/bin/env bash
# Security regression checks: every finding of docs/security/owasp-review-2026-11-23.md
# replayed as an attack that must now fail. Needs `make gateway`.
# Usage: scripts/security.sh [base-url]
set -uo pipefail
. "$(dirname "$0")/lib.sh"
BASE="${1:-$BASE}"
pass=0 fail=0
ok()  { echo "PASS  $*"; pass=$((pass + 1)); }
bad() { echo "FAIL  $*"; fail=$((fail + 1)); }
expect() { # name got want
  if [[ "$2" == "$3" ]]; then ok "$1 ($2)"; else bad "$1: got $2, want $3"; fi
}
code() { https -o /dev/null -w '%{http_code}' "$@"; }
J='content-type: application/json'

ALICE=$(token alice) BOB=$(token bob)
IN_STOCK=$(https "$BASE/api/home" | python3 -c 'import json,sys; print(next((p["id"] for p in json.load(sys.stdin)["products"] if p["canAddToBag"]), "p-100"))')

echo "== H-1 identity comes from a verified token only"
expect "x-user-id header without a token"      "$(code "$BASE/api/me/orders" -H 'x-user-id: alice')" 401
expect "token signed by someone else"          "$(code "$BASE/api/me/orders" -H 'authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.eyJzdWIiOiJhbGljZSIsImlzcyI6Imh0dHBzOi8vbWVzaHNob3AubG9jYWxob3N0Ojg0NDMvYXV0aC9yZWFsbXMvbWVzaHNob3AiLCJhdWQiOiJtZXNoc2hvcC1hcGkiLCJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl')" 401
expect "alg=none token"                        "$(code "$BASE/api/me/orders" -H 'authorization: Bearer eyJhbGciOiJub25lIn0.eyJzdWIiOiJhbGljZSJ9.')" 401
expect "checkout without a token"              "$(code -X POST "$BASE/api/bag/checkout" -H "$J" -H "idempotency-key: $(uuid)" -d '{"items":[{"productId":"p-100","quantity":1}]}')" 401
A_ORDER=$(https -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $ALICE" -H "$J" -H "idempotency-key: $(uuid)" \
  -d "{\"items\":[{\"productId\":\"$IN_STOCK\",\"quantity\":1}]}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["order"]["orderId"])' 2>/dev/null)
[[ -n "$A_ORDER" ]] && ok "alice can buy with her token ($A_ORDER)" || bad "alice's checkout failed"
expect "bob + x-user-id: alice reads alice's order" "$(code "$BASE/api/me/orders/$A_ORDER" -H "authorization: Bearer $BOB" -H 'x-user-id: alice')" 404
LEAK=$(https "$BASE/api/me/orders" -H "authorization: Bearer $BOB" -H 'x-user-id: alice' | grep -c "$A_ORDER")
expect "bob's order list does not contain alice's order" "$LEAK" 0

echo "== M-4 TLS only, one host"
expect "unknown Host header"                   "$(code "$BASE/api/home" -H 'Host: evil.example')" 404
https --tls-max 1.1 -o /dev/null "$BASE/api/home" 2>/dev/null && bad "TLS 1.1 accepted" || ok "TLS 1.1 refused"
HSTS=$(https -D - -o /dev/null "$BASE/api/home" | grep -ci '^strict-transport-security: max-age=31536000')
expect "HSTS header" "$HSTS" 1
kubectl --context "$CTX" -n istio-ingress port-forward svc/istio-ingressgateway 18080:80 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT; sleep 2
LOC=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: meshshop.localhost' http://127.0.0.1:18080/api/home)
expect "plain HTTP redirects to HTTPS" "$LOC" "301 https://meshshop.localhost/api/home"

echo "== M-3 / M-2 business limits"
Q=4611686018427387904
expect "4 x 2^62 + 1 units (overflow)" "$(code -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $ALICE" -H "$J" -H "idempotency-key: $(uuid)" \
  -d "{\"items\":[{\"productId\":\"p-100\",\"quantity\":$Q},{\"productId\":\"p-100\",\"quantity\":$Q},{\"productId\":\"p-100\",\"quantity\":$Q},{\"productId\":\"p-100\",\"quantity\":$Q},{\"productId\":\"p-100\",\"quantity\":1}]}")" 400
expect "11 units of one product"       "$(code -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $ALICE" -H "$J" -H "idempotency-key: $(uuid)" -d '{"items":[{"productId":"p-100","quantity":11}]}')" 400
expect "duplicate lines over the limit" "$(code -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $ALICE" -H "$J" -H "idempotency-key: $(uuid)" -d '{"items":[{"productId":"p-100","quantity":6},{"productId":"p-100","quantity":5}]}')" 422
expect "checkout without Idempotency-Key" "$(code -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $ALICE" -H "$J" -d '{"items":[{"productId":"p-100","quantity":1}]}')" 400
echo "== L-7 idempotent checkout"
KEY=$(uuid); CART="{\"items\":[{\"productId\":\"$IN_STOCK\",\"quantity\":1}]}"
first=$(https -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $BOB" -H "$J" -H "idempotency-key: $KEY" -d "$CART")
again=$(https -D - -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $BOB" -H "$J" -H "idempotency-key: $KEY" -d "$CART")
id1=$(sed -n 's/.*"orderId":"\([^"]*\)".*/\1/p' <<<"$first"); id2=$(sed -n 's/.*"orderId":"\([^"]*\)".*/\1/p' <<<"$again")
[[ -n "$id1" && "$id1" == "$id2" && "$again" == *"idempotent-replayed: true"* ]] && ok "retry returns the same order ($id1)" || bad "retry created another order ($id1 vs $id2)"

echo "== L-3 unguessable ids"
[[ "$id1" =~ ^ord-[0-9a-f]{20}$ ]] && ok "order id is random ($id1)" || bad "order id looks sequential: $id1"

echo "== M-5 error detail"
OOS=$(https -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $BOB" -H "$J" -H "idempotency-key: $(uuid)" -d '{"items":[{"productId":"p-103","quantity":1}]}')
[[ "$OOS" == *insufficient_stock* && "$OOS" != *have* && "$OOS" != *want* ]] && ok "out-of-stock error hides stock levels: $OOS" || bad "out-of-stock error: $OOS"

echo "== M-2 per-customer checkout limit (uses up bob's budget for a minute)"
codes=""
for _ in 1 2 3 4 5 6; do  # sold-out product: no stock used, but each attempt counts
  codes+="$(code -X POST "$BASE/api/bag/checkout" -H "authorization: Bearer $BOB" -H "$J" -H "idempotency-key: $(uuid)" -d '{"items":[{"productId":"p-103","quantity":1}]}') "
done
[[ "$codes" == *429* ]] && ok "per-customer checkout limit ($codes)" || bad "no per-customer checkout limit ($codes)"

echo "== M-5 / L-1 responses leak nothing"
NR=$(https "$BASE/nope")
[[ "$NR" == '{"error":{"code":"not_found","message":"not found"}}' ]] && ok "Envoy 404 is a JSON envelope" || bad "Envoy 404 body: $NR"
H=$(https -D - -o /dev/null "$BASE/api/me/orders" -H "authorization: Bearer $ALICE" | tr -d '\r' | tr 'A-Z' 'a-z')
for h in "x-content-type-options: nosniff" "cache-control: no-store" "cross-origin-resource-policy: same-origin" "x-frame-options: deny"; do
  [[ "$H" == *"$h"* ]] && ok "header $h" || bad "missing header $h"
done
for h in "server:" "x-envoy-upstream-service-time:"; do
  [[ "$H" != *$'\n'"$h"* ]] && ok "no $h header" || bad "header $h present"
done

echo "== L-2 the client cannot choose the trace"
TR=$(https -D - -o /dev/null "$BASE/api/home" -H 'traceparent: 00-deadbeefdeadbeefdeadbeefdeadbeef-0123456789abcdef-01' | tr -d '\r' | sed -n 's/^traceresponse: 00-\([0-9a-f]*\)-.*/\1/p')
[[ -n "$TR" && "$TR" != deadbeef* ]] && ok "new trace started at the edge ($TR)" || bad "trace id: '$TR'"

echo "== L-5 / M-1 / L-6 cluster hardening (from a pod with no sidecar, in another namespace)"
k() { kubectl --context "$CTX" "$@"; }
k create namespace sec-probe --dry-run=client -o yaml | k apply -f - >/dev/null
k label namespace sec-probe pod-security.kubernetes.io/enforce=restricted --overwrite >/dev/null
k -n sec-probe run attacker --image=registry.istio.io/release/proxyv2:1.30.8@sha256:a40d8ac073a342b07a748e6c81675636b298cd3e9bd6a7eb14036187b052d350 \
  --restart=Never --command --overrides='{"spec":{"automountServiceAccountToken":false,"securityContext":{"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"attacker","image":"registry.istio.io/release/proxyv2:1.30.8@sha256:a40d8ac073a342b07a748e6c81675636b298cd3e9bd6a7eb14036187b052d350","command":["sleep","600"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' \
  -- sleep 600 >/dev/null
k -n sec-probe wait --for=condition=Ready pod/attacker --timeout=120s >/dev/null
BFF_IP=$(k -n meshshop get pod -l app=web-bff -o jsonpath='{.items[0].status.podIP}')
reach() { k -n sec-probe exec attacker -- curl -s -o /dev/null -m 4 -w '%{http_code}' "$@" 2>/dev/null; }
for t in "forged span -> otel-collector:4318|http://otel-collector.observability:4318/v1/traces" \
         "logstash beats :5044|telnet://meshshop-ls-beats.observability:5044" \
         "elasticsearch :9200|https://meshshop-es-http.observability:9200 -k" \
         "istiod metrics :15014|http://istiod.istio-system:15014/metrics" \
         "sidecar stats :15020 (web-bff)|http://$BFF_IP:15020/stats/prometheus" \
         "keycloak :8080|http://keycloak.identity:8080/auth/realms/meshshop" \
         "web-bff :8080 (plaintext)|http://web-bff.meshshop:8080/api/home"; do
  name=${t%%|*}; url=${t#*|}
  # shellcheck disable=SC2086
  c=$(reach $url); [[ "$c" == "000" ]] && ok "blocked: $name" || bad "reachable: $name ($c)"
done
k delete namespace sec-probe --wait=false >/dev/null

PROXY=registry.istio.io/release/proxyv2:1.30.8
PRIV=$(k -n meshshop run priv --image=$PROXY --dry-run=server --overrides='{"spec":{"containers":[{"name":"priv","image":"'$PROXY'","securityContext":{"privileged":true}}]}}' 2>&1)
[[ "$PRIV" == *violates\ PodSecurity* ]] && ok "privileged pod refused in meshshop (Pod Security: restricted)" || bad "privileged pod admitted: $PRIV"
UNTRUSTED=$(k -n meshshop run untrusted --image=docker.io/library/busybox --dry-run=server --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"untrusted","image":"docker.io/library/busybox","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' 2>&1)
[[ "$UNTRUSTED" == *meshshop-trusted-images* ]] && ok "image from an untrusted registry refused" || bad "untrusted image admitted: $UNTRUSTED"
SA=$(k -n meshshop get pod -l app=web-bff -o jsonpath='{.items[0].spec.containers[?(@.name=="app")].volumeMounts[*].mountPath}')
[[ "$SA" != *serviceaccount* ]] && ok "no Kubernetes API token in app containers" || bad "SA token mounted: $SA"

echo "== M-2 edge rate limit (last: it drains the gateway's token bucket)"
R=$(seq 400 | xargs -P 40 -I{} curl -s --cacert "$CACERT" -o /dev/null -w '%{http_code}\n' "$BASE/api/home" | sort | uniq -c | tr '\n' ' ')
[[ "$R" == *429* ]] && ok "burst of 400 is throttled: $R" || bad "no rate limit: $R"

echo; echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
