#!/usr/bin/env bash
# Generates realistic traffic through the gateway so Kibana has something to
# show: browsing, product views, checkouts (some fail on stock, some on unknown
# or discontinued products), order lookups, unauthenticated calls and bad tokens.
# Usage: scripts/traffic.sh [base-url] [iterations]
set -uo pipefail
. "$(dirname "$0")/lib.sh"
BASE="${1:-$BASE}"
N="${2:-30}"
PRODUCTS=(p-100 p-101 p-102 p-103 p-104 p-999)
declare -A TOKENS=([alice]="$(token alice)" [bob]="$(token bob)")
USERS=(alice bob)

call() { https -o /dev/null -w '%{http_code}' "$@"; }
declare -A codes
count() { codes[$1:$2]=$((${codes[$1:$2]:-0} + 1)); }

for ((i = 1; i <= N; i++)); do
  user=${USERS[RANDOM % ${#USERS[@]}]}
  auth="authorization: Bearer ${TOKENS[$user]}"
  p1=${PRODUCTS[RANDOM % ${#PRODUCTS[@]}]}
  p2=${PRODUCTS[RANDOM % 3]}

  count home "$(call "$BASE/api/home")"
  count product "$(call "$BASE/api/products/$p1")"

  body="{\"items\":[{\"productId\":\"$p1\",\"quantity\":$((RANDOM % 3 + 1))},{\"productId\":\"$p2\",\"quantity\":1}]}"
  count checkout "$(call -X POST "$BASE/api/bag/checkout" -H "$auth" -H "idempotency-key: $(uuid)" \
    -H 'content-type: application/json' -d "$body")"

  count orders "$(call "$BASE/api/me/orders" -H "$auth")"
  if ((i % 7 == 0)); then
    count orders-anon "$(call "$BASE/api/me/orders")"
    count orders-badtoken "$(call "$BASE/api/me/orders" -H 'authorization: Bearer eyJhbGciOiJSUzI1NiJ9.e30.c2ln')"
  fi
  printf '.'
  sleep 0.1   # stay under the gateway's checkout rate limit
done
echo
for k in $(printf '%s\n' "${!codes[@]}" | sort); do printf '  %-20s %s\n' "$k" "${codes[$k]}"; done
