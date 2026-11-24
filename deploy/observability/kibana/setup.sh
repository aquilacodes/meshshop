#!/usr/bin/env bash
# Imports meshshop's Kibana saved objects (data view, Discover sessions, Lens
# charts, dashboard), makes the meshshop data view the default, and sets log
# retention. Runs as the least-privileged "meshshop-setup" user. Idempotent.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NS=observability
PORT="${KIBANA_SETUP_PORT:-15601}"
ES_PORT="${ES_SETUP_PORT:-19200}"
RETENTION="${LOG_RETENTION:-7d}"

secret() { kubectl -n "$NS" get secret meshshop-setup-user -o jsonpath="{.data.$1}" | base64 -d; }
# Credentials go to curl through a config file descriptor, never on the command
# line, so they don't show up in `ps`.
CURL_AUTH="user = \"$(secret username):$(secret password)\""

ES_CA=$(mktemp)
kubectl -n "$NS" get secret meshshop-es-http-certs-public -o jsonpath='{.data.ca\.crt}' | base64 -d >"$ES_CA"
kubectl -n "$NS" port-forward svc/meshshop-kb-http "$PORT:5601" >/dev/null 2>&1 &
PF=$!
kubectl -n "$NS" port-forward svc/meshshop-es-http "$ES_PORT:9200" >/dev/null 2>&1 &
PF_ES=$!
trap 'kill $PF $PF_ES 2>/dev/null || true; rm -f "$ES_CA"' EXIT
for _ in $(seq 1 30); do
  curl -s -o /dev/null "http://localhost:$PORT/api/status" && break
  sleep 1
done

kb() { curl -s -K <(printf '%s\n' "$CURL_AUTH") -H 'kbn-xsrf: meshshop' "$@"; }
# Elasticsearch, verified against ECK's CA (the certificate names the service, hence --resolve).
es() {
  curl -s -K <(printf '%s\n' "$CURL_AUTH") --cacert "$ES_CA" \
    --resolve "meshshop-es-http.$NS.svc:$ES_PORT:127.0.0.1" -H 'content-type: application/json' \
    -X "$1" "https://meshshop-es-http.$NS.svc:$ES_PORT$2" "${@:3}"
}

for _ in $(seq 1 30); do   # the Elasticsearch port-forward
  [[ "$(es GET / -o /dev/null -w '%{http_code}')" == 200 ]] && break
  sleep 1
done

# Replicas: 0 on a single node, 1 as soon as there is a second one. Without
# this, every new data stream index has an unassignable replica (cluster yellow).
for t in logs metrics traces; do
  es PUT "/_component_template/$t@custom" -d '{"template":{"settings":{"index.auto_expand_replicas":"0-1"}},"_meta":{"managed_by":"meshshop kibana/setup.sh"}}' |
    grep -q '"acknowledged":true' || { echo "  failed to create $t@custom"; exit 1; }
done
es PUT "/logs-*,metrics-*,traces-*/_settings?expand_wildcards=all" -d '{"index.auto_expand_replicas":"0-1"}' | grep -q '"acknowledged":true' ||
  { echo "  failed to update replica settings"; exit 1; }
echo "Replicas: auto-expand 0-1 on logs-*, metrics-*, traces-*"

echo "Log retention for logs-meshshop.*: $RETENTION"
# A template just for our data streams: same mappings as the built-in logs
# template, but managed by the data stream lifecycle with a retention period
# (the built-in "logs" ILM policy never deletes).
es PUT /_index_template/logs-meshshop -d @- <<JSON | python3 -c 'import json,sys; r=json.load(sys.stdin); print("  template:", r); sys.exit(0 if r.get("acknowledged") else 1)'
{
  "index_patterns": ["logs-meshshop.*-*"],
  "data_stream": {},
  "priority": 250,
  "composed_of": ["logs@mappings", "logs@settings", "logs@custom", "ecs@mappings"],
  "ignore_missing_component_templates": ["logs@custom"],
  "template": {
    "settings": { "index.lifecycle.prefer_ilm": false },
    "lifecycle": { "data_retention": "$RETENTION" }
  },
  "_meta": { "managed_by": "meshshop kibana/setup.sh", "description": "meshshop logs: personal data, keep $RETENTION" }
}
JSON

# Data streams created before the template existed: same lifecycle, now.
es PUT "/_data_stream/logs-meshshop.*/_lifecycle" -d "{\"data_retention\": \"$RETENTION\"}" >/dev/null
es PUT "/logs-meshshop.*/_settings" -d '{"index.lifecycle.prefer_ilm": false}' >/dev/null

echo "Importing saved objects..."
kb -X POST "http://localhost:$PORT/api/saved_objects/_import?overwrite=true" \
  -F "file=@$DIR/meshshop.ndjson" | python3 -c '
import json, sys
r = json.load(sys.stdin)
print("  success=%s imported=%s" % (r.get("success"), r.get("successCount")))
for e in r.get("errors", []):
    print("  ERROR", e.get("type"), e.get("id"), e.get("error"))
sys.exit(0 if r.get("success") else 1)'

kb -X POST "http://localhost:$PORT/api/data_views/default" -H 'content-type: application/json' \
  -d '{"data_view_id":"meshshop-logs","force":true}' >/dev/null
echo "Kibana ready: make kibana, then http://localhost:5601 (read-only user: make kibana-password)"
