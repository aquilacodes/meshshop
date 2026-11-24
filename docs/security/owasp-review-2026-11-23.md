# OWASP review: meshshop (2026-11-23)

**Scope:** the 5 Go services, the Istio mesh config, the Kubernetes manifests, the ELK/APM stack,
and the Postman/scripts tooling, as deployed on the local `meshshop` minikube cluster.

**Standards:** OWASP Top 10 (2021) and OWASP API Security Top 10 (2023).

**Status: all findings remediated and re-tested on 2026-11-23.** See [Remediation](#remediation)
at the end; `make security` replays every attack below as a regression test.

**Method**

| Technique | Tool / approach | Result |
|---|---|---|
| Dynamic application scan (DAST) | OWASP ZAP `zap-api-scan` (active scan, alpha rules on), driven by an OpenAPI description of the BFF, through the Istio gateway | 121 rules passed, 0 FAIL, 2 WARN |
| Targeted manual tests | curl probes from outside: identity, BOLA, overflow, limits, traversal, verbs, headers, CORS, rate, trace/request-id trust, error leakage | 9 issues confirmed |
| Lateral-movement tests | a throwaway pod in a namespace without a sidecar, probing the mesh, the observability stack and istiod | 3 issues confirmed |
| Dependency scan (SCA) | `govulncheck` (Go module + toolchain) | 19 reachable stdlib vulns, 6 more not reachable |
| Misconfiguration scan | Trivy `config` over the rendered kustomize output and the Dockerfile | 30 findings, 14 distinct checks |
| Code and config review | all of `internal/`, `services/`, `deploy/`, `scripts/` | see below |

## Summary

| ID | Finding | OWASP | Severity | Status |
|---|---|---|---|---|
| [H-1](#h-1) | Caller identity is a client-supplied header (`x-user-id`) | A01, A07 · API1, API2 | **High** (Critical once real users exist) | Fixed |
| [H-2](#h-2) | Go toolchain 1.26.5: 19 reachable stdlib CVEs; `x/net` v0.59.0 | A06 | **High** | Fixed |
| [M-1](#m-1) | Telemetry pipeline accepts unauthenticated data from any pod (forged spans, forged logs) | A08, A09 | **Medium** | Fixed |
| [M-2](#m-2) | No rate limiting or business-flow limits: one scanner bought all stock | A04 · API4, API6 | **Medium** | Fixed |
| [M-3](#m-3) | Integer overflow in cart merge: 1.8×10¹⁹ units became 1 | A04 · API4 (CWE-190) | **Medium** | Fixed |
| [M-4](#m-4) | No TLS at the edge; gateway accepts any Host | A02, A05 | **Medium** | Fixed |
| [M-5](#m-5) | Internal error details are passed to clients | A05 · API8 (CWE-209) | **Medium** | Fixed |
| [L-1](#l-1) | Missing security response headers; Envoy fingerprinting headers | A05 · API8 | Low | Fixed |
| [L-2](#l-2) | Client-supplied `traceparent` trusted at the edge | A08, A09 | Low | Fixed |
| [L-3](#l-3) | Sequential, guessable order and reservation IDs | API1 (defence in depth) | Low | Fixed |
| [L-4](#l-4) | Identity values not validated (length, charset) | A03 | Low | Fixed |
| [L-5](#l-5) | Kubernetes hardening gaps (SA tokens, PSA, NetworkPolicy, image pinning, collector securityContext) | A05, A08 | Low | Fixed (signing: accepted, see below) |
| [L-6](#l-6) | Observability access hygiene (superuser everywhere, open metrics ports) | A05, A07 | Low | Fixed |
| [L-7](#l-7) | Checkout is not idempotent; reservations never expire | A04 · API6 | Low | Fixed (saga persistence: accepted) |
| [L-8](#l-8) | Personal data in logs/traces with default retention | A09 (privacy) | Low | Fixed |
| [I-1](#i-1) | `make debug-ports` reaches domain services around the mesh's authz | A01 | Info | Accepted (documented, warned) |

Severities assume this is heading for a real deployment. Several items (H-1, M-4, L-6) are
deliberate shortcuts already called out in the README, but they're listed because they would
be the first things exploited.

---

## Findings

<a id="h-1"></a>
### H-1 · Caller identity is a client-supplied header
**OWASP:** A01 Broken Access Control, A07 Identification & Authentication Failures ·
API1 BOLA, API2 Broken Authentication

The BFF takes the caller's identity from the `x-user-id` request header (`authed()` in
`services/bff/web/main.go`). Nothing authenticates it, and the gateway forwards it unchanged.
Anyone can act as anyone.

**Evidence**
```
$ curl -H 'x-user-id: alice' localhost:8088/api/me/orders
{"orders":[{"orderId":"ord-1023","status":"confirmed","total":"$12.99",...
```
The same header lets an attacker place orders as any customer.

**Fix**
1. Add an Istio `RequestAuthentication` (JWT, issuer + JWKS) on the ingress gateway.
2. Add an `AuthorizationPolicy` that requires `requestPrincipals: ["*"]` for `/api/me/*` and `/api/bag/*`.
3. On the edge VirtualService, strip any client-sent identity: `headers.request.remove: [x-user-id]`.
4. Then either inject the subject with `outputClaimToHeaders`, or have the BFF read the
   verified claim (`x-jwt-payload` / `forwardOriginalToken`).
5. Keep the order-ownership check in the business layer. It already works: other users get 404.

<a id="h-2"></a>
### H-2 · Vulnerable Go toolchain and `golang.org/x/net`
**OWASP:** A06 Vulnerable and Outdated Components

`govulncheck` reports **19 vulnerabilities in reachable code paths**. All are in the standard
library, because the binaries are built with go1.26.5, and all are fixed in go1.26.11. They're
mostly in `net/http`, `crypto/tls`, `crypto/x509` (distrust of compromised intermediates),
`net/textproto` and `net/url`, and include HTTP/2 flow-control and CPU-exhaustion issues
(GO-2026-6611, -6612). 6 more are in imported but unreachable code, including
`golang.org/x/net` v0.59.0 (html, fixed in v0.62.0).

The root cause is that the Dockerfile uses the floating tag `golang:1.26-alpine`, and the
locally cached copy of that image is go1.26.5. The host toolchain is also go1.26.5.

**Fix**
- Pin the builder by version and digest, e.g. `golang:1.26.11-alpine@sha256:…`.
- Add `toolchain go1.26.11` to `go.mod`.
- Run `go get golang.org/x/net@latest`.
- Add a `make vuln` target that runs `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
  and fails the build on findings.

<a id="m-1"></a>
### M-1 · Telemetry pipeline accepts unauthenticated data
**OWASP:** A08 Software & Data Integrity Failures, A09 Logging & Monitoring Failures

The `observability` namespace has no sidecars and no NetworkPolicies. From a pod in an
unrelated namespace:

| Target | Result |
|---|---|
| OTel Collector `:4318/v1/traces` | **200, forged span accepted.** It appears in Kibana APM as service `shopping`, transaction "FORGED by attacker" |
| Logstash beats `:5044` | **Open, no TLS, no auth.** Any pod can write arbitrary documents into `logs-meshshop.*` |
| APM Server `:8200` | 401 without the token (good), but the collector sends the token over plain HTTP |
| Elasticsearch `:9200` | 401 (good) |

An attacker with any foothold in the cluster can hide activity, forge audit trails or flood the
backend. The RBAC-denial dashboard can no longer be trusted.

**Fix**
- Add NetworkPolicies in `observability` so that:
  - the collector accepts only from the `meshshop` and `istio-ingress` namespaces;
  - Logstash `:5044` accepts only from Filebeat pods;
  - Elasticsearch accepts only from ECK-managed components.
- Enable TLS with client certificates on the beats input (ECK can issue them).
- Turn APM Server TLS back on, or run the collector→APM hop through the mesh.

<a id="m-2"></a>
### M-2 · No rate limiting or business-flow limits
**OWASP:** A04 Insecure Design · API4 Unrestricted Resource Consumption,
API6 Unrestricted Access to Sensitive Business Flows

- Of 400 requests (40 in parallel) to `/api/home`, all 400 returned 200. Nothing limits rate at
  the gateway or in the BFF.
- **During the ZAP scan, the scanner bought the entire inventory.** No limits on quantity per
  line, lines per cart, or orders per user or time window.

**Fix**
- Add an Envoy local rate limit at the gateway: an `EnvoyFilter` with `local_ratelimit`, per IP,
  and per user once H-1 is fixed. Use stricter limits on `POST /api/bag/checkout`.
- Enforce business limits in the business layer, for example quantity 1–10 per line,
  at most 50 lines, and at most N checkouts per customer per minute.

<a id="m-3"></a>
### M-3 · Integer overflow in cart merge
**OWASP:** A04 Insecure Design · API4 (CWE-190)

`App.Checkout` sums duplicate lines with `qty[id] += it.Quantity` and checks only each line,
not the sum. Four lines of 2⁶² plus one line of 1 add up to 2⁶⁴ + 1, which wraps to **1**:

```
POST /api/bag/checkout  items: 4 × {p-100, 4611686018427387904} + {p-100, 1}
→ 201 {"order":{"orderId":"ord-1024","status":"confirmed","total":"$12.99","itemCount":1}}
```

Today the effect is benign, because the order, the reservation and the price all use the wrapped
value consistently. But the validation was bypassed, and a future change (for example, pricing
lines before merging) would turn this into a pricing bug. `inventory.Store.Reserve` merges the
same way.

**Fix:** set an upper bound on `quantity` at the BFF and business layers (see M-2), and use
checked addition when merging, in both the business and inventory layers. Add a unit test for
this input.

<a id="m-4"></a>
### M-4 · No TLS at the edge; any Host accepted
**OWASP:** A02 Cryptographic Failures, A05 Security Misconfiguration

The gateway listens only on plain HTTP :80, with `hosts: ["*"]`, and `Host: evil.example` is
served with 200. Once real auth exists (H-1), tokens would travel in clear text. Inside the mesh,
mTLS is STRICT and verified (good).

**Fix:** add an HTTPS server on the Gateway (cert-manager), set `httpsRedirect: true` on :80,
replace `*` with the real hostname, and add HSTS (see L-1).

<a id="m-5"></a>
### M-5 · Internal error details reach clients
**OWASP:** A05 · API8 Security Misconfiguration (CWE-209 information exposure)

The BFF passes business and domain errors straight to the client
(`platform.WriteError(w, err)` in every BFF handler). Examples:

- `insufficient stock: p-103 (want 1, have 0)`: exposes stock levels.
- `{"code":"upstream_error","message":"no healthy upstream"}`: exposes the Envoy topology
  (seen with `shopping` scaled to 0).
- `platform.ErrUpstream` includes the internal URL and dial error
  (`POST http://shopping:8080/v1/checkouts: dial tcp …`) for transport failures.
- `WriteError` turns any non-`*Error` into a 500 containing `err.Error()`.

**Fix:** the BFF should map errors to a small set of client-facing codes and messages
(`out_of_stock`, `unavailable`, `try_again`), with no internal identifiers. Log the details
server-side; the trace ID already links them. Make `WriteError`'s fallback a generic
"internal error".

<a id="l-1"></a>
### L-1 · Missing security response headers; fingerprinting headers
**OWASP:** A05 · API8 · ZAP 10021, 90004

Responses include only `content-type`, `date`, `content-length`,
`x-envoy-upstream-service-time` and `server: istio-envoy`. Missing:

- `X-Content-Type-Options: nosniff` (ZAP 10021)
- `Cross-Origin-Resource-Policy: same-origin` (ZAP 90004)
- `Cache-Control: no-store` on personal data such as `/api/me/*` and checkout
  (ZAP: "Storable and Cacheable Content")
- `Strict-Transport-Security`, once TLS exists

**Fix:** add the headers in a BFF middleware, or with `headers.response.set` on the edge
VirtualService. Remove `x-envoy-upstream-service-time` and rewrite `server` at the gateway.

<a id="l-2"></a>
### L-2 · Client-supplied trace context is trusted
**OWASP:** A08, A09

A client-chosen `traceparent` (`deadbeef…`) was continued by the gateway: 12 log lines and
9 spans carry the attacker's trace ID, and the client also controls the sampling flag. That
allows collisions with or poisoning of real traces, and forced sampling. The Postman collection
relies on this behaviour. A forged `x-request-id` is correctly replaced at the edge.

**Fix:** in production, drop inbound `traceparent`/`tracestate` at the edge so the gateway
always starts a new trace. Keep client-supplied trace context for dev only.

<a id="l-3"></a>
### L-3 · Sequential IDs
**OWASP:** API1 (defence in depth)

Order IDs are `ord-1001`, `ord-1002`, … and reservations are `res-1`, … . Enumeration is
blocked today by the ownership check (404 for other users' orders), but guessable IDs leak
volume and widen the impact of any future authorization bug.

**Fix:** use random or UUIDv7 IDs.

<a id="l-4"></a>
### L-4 · Identity values not validated
**OWASP:** A03 Injection (input validation)

`x-user-id` values such as `../../v1/products`, `alice/../bob`, `a%2Fb` and 5 000-character
strings are all accepted (200). Downstream paths are built with `url.PathEscape`, so no
traversal was possible; every traversal and edge-bypass probe returned 404/400. These values
are still stored as customer IDs and written to logs and traces.

**Fix:** validate identities against a strict pattern (e.g. `^[a-z0-9-]{1,64}$`), or rely on
JWT subjects once H-1 is fixed.

<a id="l-5"></a>
### L-5 · Kubernetes hardening gaps
**OWASP:** A05, A08 · Trivy KSV-0014/0012/0001/0104/0125/0020/0021, DS-0026

The app pods are well hardened: non-root, read-only root filesystem, `drop: [ALL]`,
`seccompProfile: RuntimeDefault`, `FROM scratch`. Gaps:

- `kube-api-access` service-account tokens are mounted in every app pod. They're unused, and
  Istio has its own `istio-token`. Set `automountServiceAccountToken: false`.
- There are no Pod Security Admission labels. Set `pod-security.kubernetes.io/enforce: restricted`
  on `meshshop` (this needs Istio CNI, because `istio-init` requires `NET_ADMIN`), and `baseline`
  on `observability`.
- There is no NetworkPolicy anywhere: L3 is default-allow, and only Istio's L7 policy protects
  `meshshop` (see M-1).
- The `otel-collector` Deployment has no `securityContext` (Trivy: root, writable rootfs,
  privilege escalation, no seccomp).
- Images are referenced by mutable tags (`meshshop/*:dev`, `otel/opentelemetry-collector:0.165.0`,
  Elastic `9.5.6`) rather than digests, and none are signed. Pin by digest and sign the app
  images (cosign); for production, add an admission policy that allows only trusted registries.
- Trivy's "ConfigMap with secrets" (KSV-0109) on `otel-collector` is a **false positive**: the
  value is the `${env:APM_SECRET_TOKEN}` reference.

<a id="l-6"></a>
### L-6 · Observability access hygiene
**OWASP:** A05, A07

- Kibana and Elasticsearch are used with the `elastic` **superuser**, both by people and by
  `kibana/setup.sh`. Create a read-only `meshshop-viewer` role for humans and an API key for the
  setup script.
- `kibana/setup.sh` passes the password on the curl command line, where other local users can
  see it in `ps`. Use `--netrc-file` or an API key header from a file.
- Reachable from any pod without credentials: istiod `:15014/metrics` and sidecar
  `:15020/stats/prometheus` (metrics and topology disclosure). Kibana `/api/status` shows only
  `available`. Istiod's `/debug/*` (401) and Envoy admin `:15000` (unreachable) are correctly
  protected.
- `cluster.routing.allocation.disk.threshold_enabled: false` on Elasticsearch: a full disk
  corrupts the logging pipeline instead of applying back-pressure.

<a id="l-7"></a>
### L-7 · Checkout not idempotent; reservations never expire
**OWASP:** A04 · API6

A client retry after a timeout creates a second order. A crash between "reserve" and "commit"
leaves stock held forever. This can be abused, along with M-2, to lock up inventory without
paying.

**Fix:** require an `Idempotency-Key` on checkout, give held reservations a TTL with a reaper,
and persist saga state (outbox or workflow engine), as the README already notes.

<a id="l-8"></a>
### L-8 · Personal data in telemetry
**OWASP:** A09 (privacy)

Customer IDs are written to app logs (`customer_id`), span attributes (`enduser.id`) and
request paths in Envoy access logs, with the default `logs`/`traces-apm` lifecycles. Once real
users exist, classify this data, hash or drop identifiers you don't need, and set explicit
retention through ILM or the data-stream lifecycle.

<a id="i-1"></a>
### I-1 · Debug port-forwards go around mesh authorization

`make debug-ports` and the Postman debug folders call domain services through
`kubectl port-forward`, which skips the sidecar and therefore every AuthorizationPolicy. That's
acceptable for dev, but anyone with `pods/portforward` RBAC on `meshshop` can bypass the
layering. Restrict that verb in shared clusters.

---

## What held up

- **Injection and the ZAP active scan:** 121 rules passed with no FAIL results. No SQL, command,
  path or header injection, no XSS, and reflected input is JSON-escaped (`<script>`).
- **Layering:** `make layering` passes 9/9 (layer-skipping, sideways and upward calls get 403).
  Plaintext calls to mesh services from outside the mesh are reset because mTLS is STRICT.
- **Edge exposure:** only `/api/*` is routed. Traversal and encoding tricks (`..%2F`, `%2e%2e`,
  `/api/../healthz`, `%00`, `/v1/*`) return 404/400. `TRACE`, `OPTIONS`, `PUT`, `DELETE` and
  `PATCH` return 405. No permissive CORS.
- **Input handling:** 1 MB body limit (a 2 MB body gets 400), unknown JSON fields are rejected
  (so clients can't send prices), negative and float quantities are rejected, and prices always
  come from the catalog.
- **Object-level authorization:** other customers' orders return 404, not 403, so their
  existence isn't revealed.
- **Platform:** a forged `x-request-id` is replaced at the edge; egress is `REGISTRY_ONLY`;
  secrets live in Kubernetes Secrets, with none hard-coded in the repo; Envoy admin and istiod
  debug endpoints are not exposed.

## Fix order

1. **H-2:** bump Go to 1.26.11 and `x/net`, pin the builder image, add `make vuln`. Small change, removes 19 CVEs.
2. **M-3, M-2 (business limits), M-5, L-1:** code changes in `platform`, `shopping` and `web-bff`, with unit tests.
3. **M-1, L-5:** NetworkPolicies, collector `securityContext`, `automountServiceAccountToken: false`.
4. **H-1, M-4, L-2:** JWT at the gateway, TLS, and edge header hygiene. This needs an identity provider decision.
5. **L-3, L-4, L-6, L-7, L-8:** as the project moves towards real data.

## Reproducing

- **ZAP:**
  `docker run --rm --network host -v <dir>:/zap/wrk ghcr.io/zaproxy/zaproxy:stable zap-api-scan.py -t openapi.yaml -f openapi -O http://localhost:8088 -a`
  (needs an OpenAPI description of the five `/api` operations).
- **govulncheck:** `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
- **Trivy:**
  `kubectl kustomize deploy/k8s > m.yaml && docker run --rm -v $PWD:/src aquasec/trivy config /src`
- **Layering:** `make layering`

---

<a id="remediation"></a>
## Remediation (2026-11-23)

Identity provider chosen: **Keycloak 26.8** (OpenID Connect), in its own `identity` namespace
inside the mesh. It's the de facto self-hosted OIDC server, works with Istio's
`RequestAuthentication` unchanged, and can be swapped for a managed IdP (Entra ID, Auth0,
Okta, Cognito) by changing the issuer and JWKS URL.

| ID | Fix | Where |
|---|---|---|
| H-1 | Keycloak realm `meshshop` (brute-force protection, 12+ character passwords, 5-minute access tokens, refresh-token rotation). Clients: `meshshop-web` (auth code + PKCE S256) and `meshshop-cli` (password grant, dev only). Tokens carry audience `meshshop-api`. The gateway validates every presented token (signature, issuer, audience, expiry) and denies `/api/me/*` and `/api/bag/*` without one. The BFF verifies the token again (go-oidc, RS256 only, `typ=Bearer`) and uses `sub` as the customer id. `x-user-id` is stripped at the edge and ignored by the BFF (`Service.Edge()`). | `deploy/identity/`, `deploy/istio/edge/jwt.yaml`, `services/bff/web/auth.go` |
| H-2 | `toolchain go1.26.11` in `go.mod`; builder pinned to `golang:1.26.11-alpine@sha256:…` with `GOTOOLCHAIN=local`; `x/net` v0.63.0 and other dependencies updated; `make vuln`. | `go.mod`, `Dockerfile`, `Makefile` |
| M-1 | Calico CNI, so NetworkPolicies are enforced. The collector accepts only `meshshop` and the gateway; Logstash only Filebeat; APM only the collector; Elasticsearch only Elastic components. Filebeat → Logstash uses mutual TLS (client certificate required). Collector → APM Server uses TLS (ECK CA). | `deploy/observability/stack/network-policies.yaml`, `logstash/meshshop.conf`, `agents/filebeat.yaml`, `otel-collector.yaml` |
| M-2 | Edge token-bucket rate limit (100 burst, 50/s per gateway pod) and a tighter checkout bucket (20 burst, 5/s); per-customer limit of 5 checkouts a minute in the BFF; business limits of 10 units per product and 20 lines per cart. | `deploy/istio/edge/hardening.yaml`, `services/bff/web/limits.go`, `services/business/shopping/app.go` |
| M-3 | Checked addition when merging lines (shopping, inventory), bounded quantities in every layer, overflow check on order totals. Unit tests cover 4 × 2⁶² + 1. | `shopping/app.go`, `inventory/inventory.go`, `order/order.go` |
| M-4 | HTTPS only (TLS ≥ 1.2, local CA via `scripts/certs.sh`), HTTP → 301, one host (`meshshop.localhost`; unknown hosts get 404), HSTS. | `deploy/k8s/mesh/gateway.yaml` |
| M-5 | `platform.Error` gained a `Cause` that is logged, never serialised. `WriteError` hides unknown errors. The BFF passes through only known business codes and maps everything else to `unavailable`/`internal`. Envoy-generated replies (404, 401/403, 429, 5xx) are rewritten to the JSON envelope. Stock levels are no longer in messages. | `internal/platform/httpx.go`, `client.go`, `services/bff/web/main.go`, `hardening.yaml` |
| L-1 | `nosniff`, `no-store`, CORP, CSP `default-src 'none'`, `X-Frame-Options: DENY`, `Referrer-Policy` on every response; HSTS at the gateway; `server` and `x-envoy-upstream-service-time` removed. | `internal/platform/server.go`, `gateway.yaml`, `hardening.yaml` |
| L-2 | The gateway drops `traceparent`, `tracestate`, `baggage`, B3 and Jaeger headers before tracing (Envoy early header mutation), so it always starts the trace. Responses carry W3C `traceresponse`; Postman uses that for its Kibana links. | `hardening.yaml`, `internal/platform/telemetry.go` |
| L-3 | Order and reservation ids are 80-bit random (`ord-<20 hex>`). | `order/order.go`, `inventory/inventory.go` |
| L-4 | `platform.ValidID` (`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`) on customer ids, path ids, product ids and idempotency keys in the BFF, business and domain layers. | `internal/platform/httpx.go` |
| L-5 | `automountServiceAccountToken: false`; Istio CNI (no `istio-init` with NET_ADMIN), so `meshshop`, `identity` and `istio-ingress` enforce Pod Security **restricted**, `observability` **baseline**, and Filebeat (hostPath) is isolated in `observability-agents`; explicit UIDs; collector hardened (non-root, read-only root, drop ALL, seccomp); NetworkPolicies everywhere; images pinned by digest (Go builder, Keycloak, collector, probe image); a `ValidatingAdmissionPolicy` admits only `meshshop/*` and Istio images in `meshshop` and `identity`. | `deploy/k8s/`, `deploy/istio/`, `deploy/identity/`, `deploy/observability/` |
| L-6 | Humans use `meshshop-viewer` (built-in `viewer` role); `kibana/setup.sh` uses `meshshop-setup` (custom role: Kibana + `logs-meshshop.*` templates only), with credentials passed to curl through a file descriptor rather than argv; `elastic` is break-glass only (`make kibana-admin-password`). istiod `:15014` and sidecar `:15020`/`:15090` are closed by NetworkPolicy. Disk watermarks are back on, with absolute values. | `deploy/observability/`, `deploy/istio/edge/network-policies.yaml` |
| L-7 | `Idempotency-Key` required on checkout. The order domain enforces one order per (customer, key) and the saga replays it, also under concurrent duplicates (the loser compensates). A key reused with a different cart gets 422. Held reservations expire after 2 minutes (reaper plus a check on commit). | `shopping/app.go`, `order/order.go`, `inventory/inventory.go` |
| L-8 | The customer id is now the IdP's opaque subject (a UUID), not a name, and no names or emails reach telemetry. `logs-meshshop.*` is kept 7 days (data stream lifecycle); APM traces 10 days (APM default). | `kibana/setup.sh` |
| I-1 | Kept for local debugging. `make debug-ports` prints a warning, and the README says to keep `pods/portforward` out of shared-cluster roles. | `Makefile`, `README.md` |

**Accepted residual risk**
- App images aren't signed (local builds). Use cosign plus a verifying admission controller in a
  real registry.
- Saga state isn't persisted: a crash mid-checkout is cleaned up by the reservation TTL, but a
  pending order needs a reconciler.
- Rate limits are per gateway pod and per BFF replica, not global.
- Any pod in `meshshop` can still send spans to the collector (by design: they produce them).
- `meshshop-cli` allows the password grant. It's for local testing only.

### Re-test results

| Check | Before | After |
|---|---|---|
| `make security` (every attack in this report) | — | **38 / 38 pass** |
| ZAP `zap-api-scan` (authenticated, HTTPS) | 121 pass, 0 FAIL, 2 WARN | 122 pass, 0 FAIL, 1 WARN: "SQL injection" on checkout is a **false positive** (the boolean pair got 400 vs 429 from the new rate limiter; replayed by hand, both payloads return identical 400s) |
| govulncheck | 19 reachable + 6 | **0** |
| Trivy config | 30 | 12, none actionable: KSV-0125 (registries, enforced by the admission policy Trivy can't see), KSV-0109/01010 (false positives: `${…}` placeholders), DS-0026 (Kubernetes probes instead of `HEALTHCHECK`) |
| Unit tests | — | all pass, including new tests for tokens (forged, expired, wrong issuer/audience, `alg:none`, ID token), limits, overflow, idempotency races, reservation expiry, error sanitising |
| `make smoke` / `make layering` / Postman | 9/9 · 9/9 · 48 | 10/10 · 9/9 · 64 + 59 assertions |
| Telemetry | — | traces still span the gateway → web-bff → shopping → domain; logs correlate on `trace.id`; a client-chosen trace id never appears |
