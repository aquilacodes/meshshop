# meshshop

A small shop built as Go microservices on Kubernetes with an Istio service mesh,
split into three layers: **BFF → business → domain**. The mesh enforces the layering:
a service can only call the layer directly below it.

```
            client ── sign in (OIDC) ──────────────────────────┐
              │  HTTPS :443 (make gateway → https://meshshop.localhost:8443)
   ┌──────────▼───────────┐                          ┌─────────▼─────────┐
   │ istio-ingressgateway │  ns: istio-ingress       │     Keycloak      │ ns: identity
   │ TLS, JWT check, rate │  routes /api/* and the   │  realm "meshshop" │
   │ limit, header hygiene│  realm's OIDC endpoints ─▶  (users, tokens)  │
   └──────────┬───────────┘                          └───────────────────┘
══════════════│═══════════════ mTLS (STRICT) ══════════ ns: meshshop ═══
   BFF        ▼
   ┌──────────────────────┐   shapes responses for the web UI, verifies the
   │       web-bff        │   caller's access token, calls only the business layer
   └──────────┬───────────┘
   BUSINESS   ▼
   ┌──────────────────────┐   use cases spanning domains: browse (catalog+stock),
   │       shopping       │   checkout saga, order ownership. Holds no data.
   └───┬──────────┬───────┬┘
   DOMAIN         │       │
   ┌───▼────┐ ┌───▼─────┐ ┌▼───────┐   each owns one model, its invariants and data,
   │catalog │ │inventory│ │ order  │   and calls nothing else
   └────────┘ └─────────┘ └────────┘
```

## Quick start

Requires Docker, minikube, kubectl, Helm, and Go 1.26.

```bash
make up          # minikube (Calico) + Istio + ELK/APM + Keycloak + build + deploy
make gateway     # in another terminal: ingress gateway on https://meshshop.localhost:8443
make smoke       # end-to-end flow through the gateway
make security    # replays the OWASP review's attacks; all must fail
make layering    # shows the mesh allows adjacent-layer calls and blocks the rest
make traffic     # a mix of successful and failing requests for Kibana
make kibana      # in another terminal: Kibana on http://localhost:5601
make kibana-password   # read-only user "meshshop-viewer"
make users       # demo shoppers alice and bob (Keycloak passwords)
make test vuln   # unit tests, govulncheck
make help        # all targets
```

Try it (`*.localhost` resolves to your machine; the certificate is from the local CA in `.certs/`):

```bash
export CURL_CA_BUNDLE=$PWD/.certs/ca.crt
B=https://meshshop.localhost:8443
curl -s $B/api/home | jq
TOKEN=$(make -s token)            # alice's access token (make -s token AS=bob)
curl -s -XPOST $B/api/bag/checkout -H "authorization: Bearer $TOKEN" \
  -H "idempotency-key: $(uuidgen)" -d '{"items":[{"productId":"p-100","quantity":2}]}' | jq
curl -s $B/api/me/orders -H "authorization: Bearer $TOKEN" | jq
```

## Security

The project went through an OWASP Top 10 / API Security Top 10 review
(`docs/security/owasp-review-2026-11-23.md`); every finding is fixed and `make security`
replays the attacks as regression tests.

| Concern | How |
|---|---|
| Who is calling | Keycloak (OIDC, `deploy/identity/`) issues short-lived access tokens (RS256, audience `meshshop-api`). The gateway rejects invalid tokens and requires one on `/api/me/*` and `/api/bag/*` (`deploy/istio/edge/jwt.yaml`); the BFF verifies again and uses the `sub` claim as the customer id. No header can set identity. |
| Transport | HTTPS only at the edge (TLS 1.2+, HSTS, HTTP → 301); STRICT mTLS in the mesh; TLS from the collector to APM and mutual TLS from Filebeat to Logstash. |
| Abuse | Edge rate limits (global and a tighter one for checkout), a per-customer checkout limit in the BFF, and cart limits in the business layer (≤ 10 units of a product, ≤ 20 lines, checked arithmetic). |
| Retries | `Idempotency-Key` is required on checkout; a retry returns the original order. Stock held by an abandoned checkout is released after 2 minutes. |
| Errors | Clients see a small set of codes and customer-safe messages; Envoy's own errors are rewritten to the same JSON envelope. Details are logged with the trace id. |
| Headers | `nosniff`, `no-store`, CORP, CSP, `X-Frame-Options`; no `server` or Envoy timing headers; client trace headers dropped at the edge (responses carry `traceresponse`). |
| Cluster | Calico NetworkPolicies in every namespace, Pod Security `restricted` (Istio CNI removes the privileged init container), no service-account tokens in app pods, pinned image digests, least-privilege Elasticsearch users, log retention. |
| Supply chain | Go toolchain pinned (`go.mod` + builder digest); `make vuln` runs govulncheck. |

## Postman collection (`postman/`)

Import `postman/meshshop.postman_collection.json` and the `meshshop · local` environment
(`postman/meshshop.local.postman_environment.json`).

| Folder | Target | Needs |
|---|---|---|
| **Storefront (via Istio gateway)** | the public BFF API through the real path (HTTPS gateway → JWT → mTLS → AuthorizationPolicy → services). Covers happy paths, idempotent retries, stock conflicts, limits, validation, missing/forged tokens and order ownership | `make gateway` |
| **Business layer, Domain (catalog / inventory / order)** — debug | each layer's internal API, called directly. Covers saga input, all-or-nothing reservations, and the order and reservation state machines | `make debug-ports` |

- Every request has tests. Run each folder top to bottom: earlier requests save
  `inStockProductId`, `orderId`, `reservationId` and so on for later ones.
- A collection pre-request script signs in **alice** and **bob** at Keycloak and refreshes their
  tokens. Set `alicePassword` / `bobPassword` in the environment (`make users`); `make postman` does
  it for you. Trust `.certs/ca.crt` in Postman (Settings → Certificates).
- The gateway starts a new trace for every request and the response's `traceresponse` header
  names it. The Postman console prints the Kibana APM link for each request, and the
  `lastTraceUrl` / `lastLogsUrl` variables hold the trace and its logs.
- The collection is generated by `postman/generate.py`.
- The debug folders use `kubectl port-forward`, which goes straight to the pod and skips the
  sidecar. AuthorizationPolicy doesn't apply there, so use them to debug a single service,
  not to test the layering. `make layering` tests that.
- From the CLI: `make postman` (Storefront) and `make postman-debug` (debug folders) run with newman.

## Layers

| Layer | Service | Owns | May call | Called by |
|---|---|---|---|---|
| BFF | `web-bff` | View models for the web client, token verification, client-safe errors | business | ingress gateway |
| Business | `shopping` | Cross-domain use cases and rules, sagas | domain | BFFs |
| Domain | `catalog` | Products, prices, active flag | — | business |
| Domain | `inventory` | Stock levels, all-or-nothing reservations | — | business |
| Domain | `order` | Order aggregate and lifecycle (pending → confirmed/cancelled) | — | business |

Rules of thumb used here:

- **Domain** services contain only rules that can be checked with their own data:
  "stock never goes negative" (inventory), "a confirmed order can't be cancelled" (order).
- **Business** services own rules that need more than one domain: "the catalog
  price wins over the client's price", "inactive products can't be bought", "you
  can only see your own orders", and the compensation logic when checkout fails partway.
- **BFFs** contain presentation logic only: `$12.99`, "Only 3 left", "Sold out".
  A mobile app would get its own `services/bff/mobile` over the same business API.
- Each consumer declares its own view of the contracts it uses (`services/business/shopping/ports.go`,
  the types at the top of `services/bff/web/main.go`). There is no shared DTO package.
  A provider can add fields without breaking anyone.

### Checkout saga (`services/business/shopping/app.go`)

```
price cart (catalog) → reserve stock (inventory) → create pending order (order)
                     → commit reservation        → confirm order
on failure:  order create fails → release reservation
             commit fails       → cancel order + release reservation
```

Compensation runs in-process with `context.WithoutCancel`, so it still runs if
the client disconnects. For production, persist saga state (an outbox or a
workflow engine) so a crash between steps can be resumed.

## Mesh configuration (`deploy/k8s/mesh/`)

| File | What it does |
|---|---|
| `peer-authentication.yaml` | STRICT mTLS for the namespace. Plaintext is rejected. |
| `authorization.yaml` | Default `deny-all`, then one ALLOW policy per layer. Policies select pods by the `meshshop.io/layer` label and allow callers by their SPIFFE service-account principal. |
| `gateway.yaml` | Istio `Gateway` (HTTPS for `meshshop.localhost`, HTTP redirects) + `VirtualService`: `/api/*` to `web-bff`, the meshshop realm's OIDC endpoints to Keycloak. Nothing else is routed. |
| `network-policies.yaml` | L4 isolation: only port 8080, only from meshshop pods and the gateway. |
| `virtual-services.yaml` | East-west timeouts. GETs retry on connect failures and 5xx gateway errors. Writes have `attempts: 0`, because Istio's default policy also retries POSTs. |
| `destination-rules.yaml` | `ISTIO_MUTUAL`, connection pools, outlier detection (ejects a pod after 5 consecutive 5xx), and a `v1` subset ready for canaries. |

Istio is installed via Helm (`deploy/istio/install.sh`, version 1.30.8) with the CNI node
agent (no privileged init containers), low resource requests, access logging to stdout,
`REGISTRY_ONLY` egress, and `holdApplicationUntilProxyStarts`. `deploy/istio/edge/` holds the
gateway's JWT rules, an `EnvoyFilter` (rate limits, header hygiene, JSON error bodies) and
NetworkPolicies for istiod and the gateway.

## Observability: ELK + Elastic APM (`deploy/observability/`)

```
LOGS
  app stdout (JSON) ──┐
  Envoy access logs ──┼─▶ Filebeat ──▶ Logstash ──────────▶ Elasticsearch ──▶ Kibana
  istiod (text) ──────┘   DaemonSet,   parse, ECS fields,   logs-meshshop.*    Discover, dashboard
                          k8s metadata trace.id
TRACES
  gateway Envoy ──────┐
  services (OTel SDK) ┴─▶ OTel Collector ─▶ APM Server ───▶ Elasticsearch ──▶ Kibana APM
                          batching, auth                    traces-apm-*       traces, services,
                                                            metrics-apm.*      dependencies, logs
```

All of it runs in the `observability` namespace (outside the mesh; Filebeat, which needs
hostPath, in `observability-agents`) and is managed by the ECK operator (`elastic-system`):
Elasticsearch, Kibana, APM Server, Logstash and Filebeat 9.5.6, plus the OpenTelemetry
Collector 0.165. NetworkPolicies let only the producers reach each intake (collector, Logstash,
APM) and only Elastic components reach Elasticsearch. Logs are kept 7 days (`LOG_RETENTION`
in `kibana/setup.sh`), traces 10 days.

### Logging

| Data stream | Source | Notable fields |
|---|---|---|
| `logs-meshshop.app-default` | `app` containers (slog JSON) | `service.name`, `labels.layer`, `log.level`, `http.*`, `url.path`, `trace.id`, `span.id`, `error.message` |
| `logs-meshshop.envoy-default` | every `istio-proxy` (sidecars + gateway) | `envoy.direction` (inbound/outbound), `envoy.duration`, `http.response.status_code`, `envoy.response_flags`, `envoy.downstream_peer` (caller's SPIFFE id), `envoy.rbac_denied`, `trace.id` |
| `logs-meshshop.platform-default` | istiod and non-access-log proxy output | `log.level`, `istio.scope` |

- The app logs every request with `trace_id`/`span_id` from the active span (`traceLogHandler` in
  `internal/platform/telemetry.go`). Use the `*Context` slog methods so the IDs are included.
- Envoy's access log format is JSON and includes `traceparent` (`deploy/istio/istiod-values.yaml`).
  Logstash extracts `trace.id` from it, so mesh-level lines (including RBAC 403s) link to the same trace.
- All parsing lives in `deploy/observability/stack/logstash/meshshop.conf`. Logstash reloads it
  about a minute after `kubectl apply -k deploy/observability/stack`.

### Distributed tracing

- **The ingress gateway starts every trace.** A `Telemetry` resource
  (`deploy/observability/mesh/telemetry.yaml`) gives it the `otel-tracing` provider at 100% sampling.
- **The services continue the trace with the OpenTelemetry Go SDK.**
  - Server spans are named after the route (`POST /v1/checkouts`), and client spans are created
    per downstream call.
  - Each checkout saga step is its own span (`checkout.reserve_stock`, `checkout.compensate`, ...),
    and business rejections are tagged `meshshop.rejected=<code>`.
  - Configuration uses the standard `OTEL_*` environment variables, patched into every Deployment
    by `deploy/k8s/telemetry-env.yaml`.
- **The sidecars inside `meshshop` have no tracer on purpose.** They forward `traceparent`
  unchanged, so spans connect service to service and APM shows services rather than proxies.
- **Clients cannot pick the trace.** The gateway drops inbound `traceparent`/`tracestate`/B3
  headers; every response carries a W3C `traceresponse` header with the trace id instead.
- **The OpenTelemetry Collector is the only tracing endpoint.** It adds the APM secret token
  and talks TLS to APM Server.
  To switch to another backend, change only `otel-collector.yaml`.

One checkout looks like this in APM:

```
istio-ingressgateway  web-bff.meshshop.svc.cluster.local:8080/api/*
└ web-bff     POST /api/bag/checkout
  └ shopping  POST /v1/checkouts
    ├ checkout.price_cart     → catalog   GET /v1/products
    ├ checkout.reserve_stock  → inventory POST /v1/reservations
    ├ checkout.create_order   → order     POST /v1/orders
    ├ checkout.commit_stock   → inventory POST /v1/reservations/{id}/commit
    └ checkout.confirm_order  → order     POST /v1/orders/{id}/confirm
```

### In Kibana

`make kibana`, log in as the read-only `meshshop-viewer` (`make kibana-password`), then:

- **Observability → APM → Services**: latency, throughput and failure rate per service.
  - Open a transaction, then a trace sample, for the waterfall.
  - The **Logs** tab on a trace shows the app and Envoy log lines that share its `trace.id`.
  - **Dependencies** shows the calls between services.
- **Dashboards → "meshshop · Service mesh overview"**: requests by service and by layer,
  status codes, p95 latency, RBAC denials by caller identity, and app warnings/errors.
- **Discover sessions**: *App logs*, *Envoy access logs*, *RBAC denials (layer violations)*,
  *Warnings and errors*. Click a `trace.id` to filter on it.

Saved objects are generated by `deploy/observability/kibana/generate.py` (edit it, re-run it,
then run `deploy/observability/kibana/setup.sh`). The APM **Service map** needs a Platinum or
trial license. To try it locally, run
`POST /_license/start_trial?acknowledge=true` in Dev Tools.

Without Kibana:

```bash
kubectl -n meshshop logs deploy/shopping -c istio-proxy   # Envoy access log per call (JSON)
kubectl -n meshshop logs deploy/order -c app              # app JSON logs with trace_id
```

## Layout

```
internal/platform/          shared plumbing only: server lifecycle, health, logging,
                            error envelope, header-propagating client
services/bff/web/           web-bff
services/business/shopping/ ports.go (interfaces + HTTP adapters), app.go (use cases), main.go (routes)
services/domain/{catalog,inventory,order}/
                            <model>.go (types, invariants, store), main.go (routes)
deploy/istio/               Helm values (incl. JSON access log format, OTel provider, CNI) + install script,
                            edge/ (JWT rules, EnvoyFilter hardening, NetworkPolicies)
deploy/identity/            Keycloak: optimized image (Dockerfile), realm/ (clients, demo users), mesh policies
deploy/k8s/                 kustomize: namespace, apps/ (SA + Deployment + Service per service), mesh/,
                            telemetry-env.yaml (OTEL_* env patched into every Deployment)
deploy/observability/       install.sh, ECK operator values, stack/ (Elasticsearch, Kibana, APM,
                            Logstash + pipeline, OTel Collector, NetworkPolicies, roles), agents/ (Filebeat),
                            mesh/ (gateway Telemetry), kibana/ (saved-object generator + importer)
scripts/                    smoke.sh (e2e via gateway), security.sh (OWASP regression), layering.sh
                            (authz proof), traffic.sh (load mix), certs.sh (local CA), lib.sh (tokens)
postman/                    collection + environment, generate.py
Dockerfile                  one multi-stage build for all services (--build-arg SERVICE=...), FROM scratch
```

## Adding a service

1. `services/<layer>/<name>/main.go` using `platform.NewService(name, layer)`.
2. Add it to `SERVICES` in the `Makefile`.
3. Copy a file in `deploy/k8s/apps/`, set the name and the `meshshop.io/layer` label, and add it to `kustomization.yaml`.
4. If it is a new BFF or business service, add its principal to the ALLOW policy of the layer below it in `mesh/authorization.yaml`.
5. Add a VirtualService and a DestinationRule.

## Deliberate simplifications

- Data is in memory and reset when a pod restarts. Each domain service is the only
  owner of its store, so swapping in a database per service doesn't touch other layers.
- Keycloak runs one replica on an embedded H2 database; the realm and demo users are
  re-imported on each start. Use PostgreSQL and the auth-code + PKCE client (`meshshop-web`)
  for anything real, and disable the password-grant client (`meshshop-cli`).
- Certificates come from a local CA (`scripts/certs.sh`). Use cert-manager in a real cluster.
- Rate limits are per gateway pod and per BFF replica; a shared limiter (Redis, Envoy global
  rate limit service) makes them exact across replicas.
- One replica each. Scale with `kubectl scale`; DestinationRules handle load balancing and ejection.
- The Elastic stack runs as a single node with ECK's self-signed TLS inside the cluster and
  plain HTTP for Kibana on the port-forward.
- `make debug-ports` port-forwards straight into pods, past mTLS and authorization. It's for
  local debugging; keep `pods/portforward` out of shared-cluster roles.
- The Elastic stack needs about 4 GB, so `make cluster` creates the minikube node with 9 GB.
