# shellcheck shell=bash
# Shared helpers for the scripts: HTTPS to the gateway with the local CA, and
# Keycloak tokens for the demo users. Source it: . "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="${CTX:-meshshop}"
BASE="${BASE:-https://meshshop.localhost:8443}"
CACERT="${CACERT:-$ROOT/.certs/ca.crt}"
TOKEN_URL="$BASE/auth/realms/meshshop/protocol/openid-connect/token"

https() { curl -s --cacert "$CACERT" "$@"; }

# demo_password USER: from the Secret that Keycloak imported it from.
demo_password() {
  kubectl --context "$CTX" -n identity get secret meshshop-demo-users -o jsonpath="{.data.$1}" | base64 -d
}

# token USER: an access token for a demo user (password grant on the dev-only
# meshshop-cli client). The password goes to curl on stdin, not on its command line.
token() {
  demo_password "$1" | https -X POST "$TOKEN_URL" -d grant_type=password -d client_id=meshshop-cli \
    -d "username=$1" --data-urlencode password@- |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("access_token") or sys.exit("token: %s" % r))'
}

uuid() { cat /proc/sys/kernel/random/uuid; }
