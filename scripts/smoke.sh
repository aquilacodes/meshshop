#!/usr/bin/env bash
# End-to-end flow through the Istio ingress gateway (HTTPS) -> web-bff -> shopping
# -> domain, signed in with Keycloak tokens. Needs `make gateway`.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
BASE="${1:-$BASE}"
pass=0 fail=0

check() { # name expected
  if [[ "$CODE" == "$2" ]]; then
    echo "PASS  $1 ($CODE)"; pass=$((pass + 1))
  else
    echo "FAIL  $1: want $2, got $CODE"; echo "      $BODY"; fail=$((fail + 1))
  fi
}

req() { # method path [token] [json-body] [idempotency-key] -> sets CODE and BODY
  local args=(-w '\n%{http_code}' -X "$1" "$BASE$2")
  [[ -n "${3:-}" ]] && args+=(-H "authorization: Bearer $3")
  [[ -n "${4:-}" ]] && args+=(-H 'content-type: application/json' -d "$4")
  [[ -n "${5:-}" ]] && args+=(-H "idempotency-key: $5")
  local out
  out=$(https "${args[@]}")
  CODE="${out##*$'\n'}"
  BODY="${out%$'\n'*}"; BODY="${BODY%$'\n'}"
}

ALICE=$(token alice)
BOB=$(token bob)
echo "signed in alice and bob (Keycloak, realm meshshop)"

req GET /api/home
check "browse products" 200
echo "      ${BODY:0:300}"

req GET /api/products/p-104
check "discontinued product hidden" 404

req POST /api/bag/checkout "" '{"items":[{"productId":"p-100","quantity":1}]}' "$(uuid)"
check "checkout requires sign-in" 401

# Stock is in-memory and finite (and `make traffic` uses it up), so buy whatever is in stock.
IN_STOCK=$(https "$BASE/api/home" | python3 -c '
import json, sys
print(next((p["id"] for p in json.load(sys.stdin)["products"] if p["canAddToBag"]), ""))')
if [[ -z "$IN_STOCK" ]]; then
  echo "NOTE  everything is sold out; restart inventory to reset stock:"
  echo "      kubectl -n meshshop rollout restart deploy/inventory"
fi
KEY=$(uuid)
CART="{\"items\":[{\"productId\":\"${IN_STOCK:-p-100}\",\"quantity\":1}]}"
req POST /api/bag/checkout "$ALICE" "$CART" "$KEY"
check "checkout ($IN_STOCK)" 201
echo "      $BODY"
ORDER_ID=$(sed -n 's/.*"orderId":"\([^"]*\)".*/\1/p' <<<"$BODY")

req POST /api/bag/checkout "$ALICE" "$CART" "$KEY"
check "retry with the same Idempotency-Key returns the same order" 200
grep -q "$ORDER_ID" <<<"$BODY" || { echo "FAIL  replay returned a different order"; fail=$((fail + 1)); }

req POST /api/bag/checkout "$ALICE" '{"items":[{"productId":"p-103","quantity":1}]}' "$(uuid)"
check "out-of-stock checkout rejected" 409
echo "      $BODY"

req GET /api/me/orders "$ALICE"
check "list my orders" 200

req GET "/api/me/orders/$ORDER_ID" "$ALICE"
check "get my order" 200

req GET "/api/me/orders/$ORDER_ID" "$BOB"
check "cannot read someone else's order" 404

req GET /healthz
check "non-/api paths not routed at the edge" 404

echo; echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
