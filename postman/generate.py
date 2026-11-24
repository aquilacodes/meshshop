#!/usr/bin/env python3
"""Generates the meshshop Postman collection + environment (v2.1).

Usage: python3 postman/generate.py postman
"""
import json
import sys
import uuid

OUT = sys.argv[1]
NS = uuid.UUID("6f0c3c1e-6d1e-4c55-9a4e-7d3e8c2a1b00")


def uid(name):
    return str(uuid.uuid5(NS, name))


def lines(s):
    return s.strip("\n").split("\n")


def script(kind, src):
    return {"listen": kind, "script": {"type": "text/javascript", "exec": lines(src)}}


def url(raw):
    """Postman URL object from a raw string like {{baseUrl}}/api/x?y=1."""
    base, _, query = raw.partition("?")
    host, _, path = base.partition("/")
    u = {"raw": raw, "host": [host], "path": [p for p in path.split("/") if p]}
    if query:
        u["query"] = [dict(zip(("key", "value"), kv.split("=", 1))) for kv in query.split("&")]
    return u


def req(name, method, raw, tests, body=None, headers=(), desc="", pre=None):
    r = {"method": method, "header": [{"key": k, "value": v} for k, v in headers], "url": url(raw)}
    if body is not None:
        r["header"].append({"key": "Content-Type", "value": "application/json"})
        r["body"] = {"mode": "raw", "raw": json.dumps(body, indent=2),
                     "options": {"raw": {"language": "json"}}}
    if desc:
        r["description"] = desc
    events = [script("test", tests)]
    if pre:
        events.insert(0, script("prerequest", pre))
    return {"name": name, "id": uid(name), "event": events, "request": r, "response": []}


def folder(name, desc, items, pre=None):
    f = {"name": name, "id": uid("folder:" + name), "description": desc, "item": items}
    if pre:
        f["event"] = [script("prerequest", pre)]
    return f


ALICE = ("Authorization", "Bearer {{aliceToken}}")
BOB = ("Authorization", "Bearer {{bobToken}}")
IDEM = ("Idempotency-Key", "{{$guid}}")

# ---------------------------------------------------------------------------
# Collection-level scripts
# ---------------------------------------------------------------------------
COLLECTION_PRE = r"""
// Sign in alice and bob at Keycloak (password grant on the dev-only meshshop-cli
// client) and keep their access tokens fresh. Requests send
// "Authorization: Bearer {{aliceToken}}" / "{{bobToken}}".
// Passwords are not stored in the collection: `make postman` passes them in;
// in the Postman app set alicePassword / bobPassword in the environment
// (`make users` prints them).
const base = pm.variables.get('baseUrl');
const url = pm.variables.replaceIn(pm.request.url.toString());
if (url.startsWith(base)) {
    const now = Math.floor(Date.now() / 1000);
    ['alice', 'bob'].forEach(user => {
        if (Number(pm.collectionVariables.get(`${user}TokenExp`) || 0) - 30 > now) return;
        const password = pm.variables.get(`${user}Password`);
        if (!password) { console.warn(`set ${user}Password (make users) to sign in as ${user}`); return; }
        pm.sendRequest({
            url: `${base}/auth/realms/meshshop/protocol/openid-connect/token`,
            method: 'POST',
            body: { mode: 'urlencoded', urlencoded: [
                { key: 'grant_type', value: 'password' }, { key: 'client_id', value: 'meshshop-cli' },
                { key: 'username', value: user }, { key: 'password', value: password }] },
        }, (err, res) => {
            if (err || res.code !== 200) { console.error(`sign-in as ${user} failed`, err || res.text()); return; }
            const t = res.json();
            pm.collectionVariables.set(`${user}Token`, t.access_token);
            pm.collectionVariables.set(`${user}TokenExp`, now + t.expires_in);
        });
    });
}
"""

COLLECTION_TEST = r"""
// Shared checks for every request.
pm.test('responds within 2s', () => pm.expect(pm.response.responseTime).to.be.below(2000));

const isJSON = (pm.response.headers.get('Content-Type') || '').includes('application/json');
if (pm.response.code >= 400 && isJSON) {
    pm.test('error uses the standard envelope {error: {code, message}}', () => {
        const err = pm.response.json().error;
        pm.expect(err, 'error').to.be.an('object');
        pm.expect(err.code, 'error.code').to.be.a('string').and.not.empty;
        pm.expect(err.message, 'error.message').to.be.a('string');
    });
}

// Where to look in Kibana for this request (also in the Postman console). The
// gateway starts a new trace for every request (client trace headers are
// dropped) and the BFF names it in the W3C `traceresponse` header.
const tr = pm.response.headers.get('traceresponse');
const traceId = tr ? tr.split('-')[1] : '';
pm.collectionVariables.set('traceId', traceId);
if (traceId) {
    const kibana = pm.variables.get('kibanaUrl');
    const traceUrl = `${kibana}/app/apm/link-to/trace/${traceId}`;
    const logsUrl = `${kibana}/app/discover#/?_g=(time:(from:now-1h,to:now))` +
        `&_a=(query:(language:kuery,query:'trace.id:"${traceId}"'))`;
    pm.collectionVariables.set('lastTraceUrl', traceUrl);
    pm.collectionVariables.set('lastLogsUrl', logsUrl);
    console.log(`${pm.response.code} ${pm.info.requestName} → ${traceUrl}`);
}
"""

# ---------------------------------------------------------------------------
# Storefront (through the gateway)
# ---------------------------------------------------------------------------
HOME_SCHEMA = {
    "type": "object", "required": ["title", "products"],
    "properties": {
        "title": {"type": "string"},
        "products": {"type": "array", "items": {
            "type": "object", "required": ["id", "title", "subtitle", "price", "canAddToBag"],
            "properties": {
                "id": {"type": "string"}, "title": {"type": "string"}, "subtitle": {"type": "string"},
                "price": {"type": "string", "pattern": "^\\$\\d+\\.\\d{2}$"},
                "badge": {"type": "string"}, "canAddToBag": {"type": "boolean"},
            }}},
    },
}

storefront = folder(
    "Storefront (via Istio gateway)",
    "The public API: `web-bff` behind the Istio ingress gateway (`make gateway` → `{{baseUrl}}`).\n\n"
    "Run the folder top to bottom (Collection Runner or `make postman`): **Home** picks an in-stock "
    "product, **Checkout** saves `orderId`, later requests use both.\n\n"
    "Requests are signed in as **alice** (`{{aliceToken}}`) or **bob** with Keycloak access tokens, "
    "fetched by the collection's pre-request script. Checkout needs an `Idempotency-Key`.",
    [
        req("Home — product cards", "GET", "{{baseUrl}}/api/home", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
const body = pm.response.json();
pm.test('matches the web view-model schema', () => pm.response.to.have.jsonSchema(SCHEMA));
pm.test('discontinued products are hidden', () =>
    pm.expect(body.products.map(p => p.id)).to.not.include('p-104'));
pm.test('sold-out products are badged and not buyable', () => {
    body.products.filter(p => !p.canAddToBag).forEach(p => pm.expect(p.badge).to.eql('Sold out'));
});
const inStock = body.products.find(p => p.canAddToBag);
pm.test('at least one product in stock (else: kubectl -n meshshop rollout restart deploy/inventory)',
    () => pm.expect(inStock).to.be.an('object'));
if (inStock) pm.collectionVariables.set('inStockProductId', inStock.id);
""".replace("SCHEMA", json.dumps(HOME_SCHEMA)),
            desc="Product cards shaped for the web UI: formatted price, stock badge, `canAddToBag`."),

        req("Product details", "GET", "{{baseUrl}}/api/products/{{inStockProductId}}", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('is the requested product', () =>
    pm.expect(pm.response.json().id).to.eql(pm.collectionVariables.get('inStockProductId')));
"""),

        req("Product — discontinued (404)", "GET", "{{baseUrl}}/api/products/p-104", r"""
pm.test('404: inactive products are not exposed', () => pm.response.to.have.status(404));
pm.test('error code not_found', () => pm.expect(pm.response.json().error.code).to.eql('not_found'));
""", desc="`p-104` exists in the catalog but is inactive; the business layer hides it."),

        req("Checkout", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('201 Created', () => pm.response.to.have.status(201));
const { order, message } = pm.response.json();
pm.test('order is confirmed', () => pm.expect(order.status).to.eql('confirmed'));
pm.test('total is formatted for display', () => pm.expect(order.total).to.match(/^\$\d+\.\d{2}$/));
pm.test('item count matches the bag', () => pm.expect(order.itemCount).to.eql(1));
pm.test('friendly message', () => pm.expect(message).to.include('confirmed'));
pm.collectionVariables.set('orderId', order.orderId);
""", body={"items": [{"productId": "{{inStockProductId}}", "quantity": 1}]},
            headers=[ALICE, ("Idempotency-Key", "{{checkoutKey}}")],
            pre="pm.collectionVariables.set('checkoutKey', pm.variables.replaceIn('{{$guid}}'));",
            desc="Runs the checkout saga: price (catalog) → reserve stock (inventory) → create order → "
                 "commit stock → confirm order. Open the APM link in the console to see each step as a span."),

        req("Checkout — retry with the same Idempotency-Key (200)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('200: the earlier order, not a new one', () => pm.response.to.have.status(200));
pm.test('Idempotent-Replayed header', () => pm.expect(pm.response.headers.get('Idempotent-Replayed')).to.eql('true'));
pm.test('same order id', () => pm.expect(pm.response.json().order.orderId).to.eql(pm.collectionVariables.get('orderId')));
""", body={"items": [{"productId": "{{inStockProductId}}", "quantity": 1}]},
            headers=[ALICE, ("Idempotency-Key", "{{checkoutKey}}")],
            desc="A client that times out and retries with the same key gets the same order back."),

        req("Checkout — over the quantity limit (400)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('400: at most 10 of a product per order', () => pm.response.to.have.status(400));
""", body={"items": [{"productId": "p-100", "quantity": 11}]}, headers=[ALICE, IDEM]),

        req("Checkout — out of stock (409)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('409 Conflict', () => pm.response.to.have.status(409));
pm.test('error code insufficient_stock', () =>
    pm.expect(pm.response.json().error.code).to.eql('insufficient_stock'));
""", body={"items": [{"productId": "p-103", "quantity": 1}]}, headers=[ALICE, IDEM],
            desc="`p-103` is seeded with zero stock. The reservation fails and nothing is created."),

        req("Checkout — discontinued product (409)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('409 Conflict', () => pm.response.to.have.status(409));
pm.test('error code product_unavailable', () =>
    pm.expect(pm.response.json().error.code).to.eql('product_unavailable'));
""", body={"items": [{"productId": "p-104", "quantity": 1}]}, headers=[ALICE, IDEM]),

        req("Checkout — empty bag (400)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('400 Bad Request', () => pm.response.to.have.status(400));
""", body={"items": []}, headers=[ALICE, IDEM]),

        req("Checkout — unknown field (400)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('400: unknown JSON fields are rejected', () => pm.response.to.have.status(400));
""", body={"items": [{"productId": "p-100", "quantity": 1}], "price": 1}, headers=[ALICE, IDEM],
            desc="Clients can't send prices; bodies are decoded with `DisallowUnknownFields`."),

        req("Checkout — not signed in (401)", "POST", "{{baseUrl}}/api/bag/checkout", r"""
pm.test('401 Unauthorized without a token', () => pm.response.to.have.status(401));
pm.test('WWW-Authenticate: Bearer', () => pm.expect(pm.response.headers.get('WWW-Authenticate')).to.include('Bearer'));
""", body={"items": [{"productId": "p-100", "quantity": 1}]}, headers=[IDEM]),

        req("My orders — spoofed x-user-id, no token (401)", "GET", "{{baseUrl}}/api/me/orders", r"""
pm.test('401: identity comes only from a verified token', () => pm.response.to.have.status(401));
""", headers=[("x-user-id", "alice")]),

        req("My orders — forged token (401)", "GET", "{{baseUrl}}/api/me/orders", r"""
pm.test('401: the gateway rejects tokens it cannot verify', () => pm.response.to.have.status(401));
""", headers=[("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJl")]),

        req("My orders", "GET", "{{baseUrl}}/api/me/orders", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
const ids = pm.response.json().orders.map(o => o.orderId);
pm.test('includes the order just placed', () => pm.expect(ids).to.include(pm.collectionVariables.get('orderId')));
""", headers=[ALICE]),

        req("My order", "GET", "{{baseUrl}}/api/me/orders/{{orderId}}", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
const o = pm.response.json();
pm.test('is my order', () => pm.expect(o.orderId).to.eql(pm.collectionVariables.get('orderId')));
pm.test('lists items for display', () => pm.expect(o.items[0]).to.match(/^\d+ × .+/));
""", headers=[ALICE]),

        req("Someone else's order (404)", "GET", "{{baseUrl}}/api/me/orders/{{orderId}}", r"""
pm.test("404: other customers' orders are invisible", () => pm.response.to.have.status(404));
""", headers=[BOB],
            desc="Ownership is a business-layer rule: it answers 404, not 403, so order ids can't be probed."),

        req("Not routed at the edge (404)", "GET", "{{baseUrl}}/healthz", r"""
pm.test('404 from Envoy: only /api/* is routed to the BFF', () => pm.response.to.have.status(404));
pm.test('JSON error envelope, no Envoy details', () => pm.expect(pm.response.json().error.code).to.eql('not_found'));
""", desc="The gateway VirtualService only matches `/api/`. Internal endpoints such as `/healthz` "
          "are unreachable from outside (Envoy response flag `NR` in the gateway access log)."),
    ],
)

# ---------------------------------------------------------------------------
# Debug folders: direct to pods via kubectl port-forward (bypasses the mesh)
# ---------------------------------------------------------------------------
DEBUG_NOTE = ("**Debug only.** These requests go straight to the pod through `kubectl port-forward` "
              "(`make debug-ports`). They skip the sidecar, so mTLS, AuthorizationPolicy, timeouts and "
              "retries don't apply. From inside the mesh, only the layer above may call these endpoints.")

business = folder("Business layer — shopping (debug)", DEBUG_NOTE + "\n\n`{{shoppingUrl}}` → `svc/shopping`.", [
    req("Browse products", "GET", "{{shoppingUrl}}/v1/products", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('products carry stock from inventory', () => {
    const p = pm.response.json().items[0];
    pm.expect(p).to.include.keys('priceCents', 'available', 'inStock');
});
const inStock = pm.response.json().items.find(p => p.inStock);
pm.test('something is in stock (else: kubectl -n meshshop rollout restart deploy/inventory)', () => pm.expect(inStock).to.be.an('object'));
if (inStock) pm.collectionVariables.set('inStockProductId', inStock.id);
"""),
    req("Product", "GET", "{{shoppingUrl}}/v1/products/p-100", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
"""),
    req("Checkout", "POST", "{{shoppingUrl}}/v1/checkouts", r"""
pm.test('201 Created', () => pm.response.to.have.status(201));
pm.test('priced by the catalog and confirmed', () => {
    const o = pm.response.json().order;
    pm.expect(o.status).to.eql('confirmed');
    pm.expect(o.lines[0].unitPriceCents).to.be.above(0);
});
""", body={"customerId": "{{userId}}", "idempotencyKey": "{{$guid}}", "items": [{"productId": "{{inStockProductId}}", "quantity": 1}]}),
    req("Customer orders", "GET", "{{shoppingUrl}}/v1/customers/{{userId}}/orders", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('every order belongs to the customer', () =>
    pm.response.json().items.forEach(o => pm.expect(o.customerId).to.eql(pm.collectionVariables.get('userId') || pm.variables.get('userId'))));
"""),
])

catalog = folder("Domain — catalog (debug)", DEBUG_NOTE + "\n\n`{{catalogUrl}}` → `svc/catalog`.", [
    req("List products", "GET", "{{catalogUrl}}/v1/products", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('includes inactive products (filtering is a business rule)', () =>
    pm.expect(pm.response.json().items.some(p => p.active === false)).to.be.true);
"""),
    req("Get products by ids", "GET", "{{catalogUrl}}/v1/products?ids=p-100,p-102,p-999", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('unknown ids are skipped', () =>
    pm.expect(pm.response.json().items.map(p => p.id)).to.eql(['p-100', 'p-102']));
"""),
    req("Get product", "GET", "{{catalogUrl}}/v1/products/p-101", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('price is in cents', () => pm.expect(pm.response.json().priceCents).to.be.a('number'));
"""),
])

inventory = folder("Domain — inventory (debug)", DEBUG_NOTE + "\n\n`{{inventoryUrl}}` → `svc/inventory`. "
                   "Run in order: the reservation lifecycle is held → committed, and committed is final.", [
    req("Stock levels", "GET", "{{inventoryUrl}}/v1/stock?productIds=p-100,p-101,p-102,p-103,p-104", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
const items = pm.response.json().items;
pm.test('p-103 has no stock', () => pm.expect(items.find(s => s.productId === 'p-103').available).to.eql(0));
const avail = items.find(s => s.available > 0);
pm.test('something is in stock (else: kubectl -n meshshop rollout restart deploy/inventory)', () => pm.expect(avail).to.be.an('object'));
if (avail) pm.collectionVariables.set('stockProductId', avail.productId);
""", desc="Picks a product that still has stock (`stockProductId`) for the reservation requests below."),
    req("Reserve stock", "POST", "{{inventoryUrl}}/v1/reservations", r"""
pm.test('201 Created', () => pm.response.to.have.status(201));
pm.test('held', () => pm.expect(pm.response.json().status).to.eql('held'));
pm.collectionVariables.set('reservationId', pm.response.json().id);
""", body={"ref": "postman", "lines": [{"productId": "{{stockProductId}}", "quantity": 1}]}),
    req("Reserve stock — all or nothing (409)", "POST", "{{inventoryUrl}}/v1/reservations", r"""
pm.test('409: one short line fails the whole reservation', () => pm.response.to.have.status(409));
pm.test('error code insufficient_stock', () => pm.expect(pm.response.json().error.code).to.eql('insufficient_stock'));
""", body={"ref": "postman", "lines": [{"productId": "{{stockProductId}}", "quantity": 1}, {"productId": "p-103", "quantity": 1}]}),
    req("Commit reservation", "POST", "{{inventoryUrl}}/v1/reservations/{{reservationId}}/commit", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('committed', () => pm.expect(pm.response.json().status).to.eql('committed'));
"""),
    req("Commit again — idempotent", "POST", "{{inventoryUrl}}/v1/reservations/{{reservationId}}/commit", r"""
pm.test('200: repeating a transition is a no-op', () => pm.response.to.have.status(200));
"""),
    req("Release after commit (409)", "POST", "{{inventoryUrl}}/v1/reservations/{{reservationId}}/release", r"""
pm.test('409: committed is final', () => pm.response.to.have.status(409));
pm.test('error code invalid_transition', () => pm.expect(pm.response.json().error.code).to.eql('invalid_transition'));
"""),
])

order = folder("Domain — order (debug)", DEBUG_NOTE + "\n\n`{{orderUrl}}` → `svc/order`. "
               "Run in order: pending → confirmed, and confirmed is final.", [
    req("Create order", "POST", "{{orderUrl}}/v1/orders", r"""
pm.test('201 Created', () => pm.response.to.have.status(201));
const o = pm.response.json();
pm.test('pending', () => pm.expect(o.status).to.eql('pending'));
pm.test('total computed by the domain (2 × 1299 + 899)', () => pm.expect(o.totalCents).to.eql(3497));
pm.collectionVariables.set('debugOrderId', o.id);
""", body={"customerId": "postman", "currency": "USD", "reservationId": "res-none",
           "lines": [{"productId": "p-100", "name": "Ceramic Mug", "quantity": 2, "unitPriceCents": 1299},
                     {"productId": "p-103", "name": "Dotted Notebook", "quantity": 1, "unitPriceCents": 899}]},
        desc="The order domain trusts the prices it is given; pricing from the catalog is the business layer's job."),
    req("Create order — invalid (400)", "POST", "{{orderUrl}}/v1/orders", r"""
pm.test('400: an order needs a customer and lines', () => pm.response.to.have.status(400));
""", body={"currency": "USD", "lines": []}),
    req("Get order", "GET", "{{orderUrl}}/v1/orders/{{debugOrderId}}", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
"""),
    req("Orders by customer", "GET", "{{orderUrl}}/v1/orders?customerId=postman", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('contains the created order', () =>
    pm.expect(pm.response.json().items.map(o => o.id)).to.include(pm.collectionVariables.get('debugOrderId')));
"""),
    req("Confirm order", "POST", "{{orderUrl}}/v1/orders/{{debugOrderId}}/confirm", r"""
pm.test('200 OK', () => pm.response.to.have.status(200));
pm.test('confirmed', () => pm.expect(pm.response.json().status).to.eql('confirmed'));
"""),
    req("Cancel after confirm (409)", "POST", "{{orderUrl}}/v1/orders/{{debugOrderId}}/cancel", r"""
pm.test('409: confirmed is final', () => pm.response.to.have.status(409));
"""),
])

DESCRIPTION = """\
Requests for **meshshop**: a Go microservice shop on Kubernetes + Istio, with BFF → business → domain layering.

## Setup
1. `make gateway` (Istio ingress gateway, HTTPS on `https://meshshop.localhost:8443`).
   The certificate is from the local CA in `.certs/ca.crt`: add it under Settings → Certificates → CA
   certificates (newman: `--ssl-extra-ca-certs .certs/ca.crt`, as `make postman` does).
2. Sign-in: requests use Keycloak tokens for the demo users **alice** and **bob**. Put their passwords
   (`make users`) into `alicePassword` / `bobPassword` in the environment; `make postman` does it for you.
3. Optional: `make kibana` (Kibana on `localhost:5601`) to follow each request's trace and logs
4. Optional, for the *debug* folders: `make debug-ports` (port-forwards to the internal services)
5. Select the **meshshop · local** environment, or just use the collection variables

## Folders
- **Storefront (via Istio gateway)**: the public API, through the real path
  (HTTPS gateway → JWT check → mTLS → AuthorizationPolicy → web-bff → shopping → domain). Run it in order.
- **Business / Domain (debug)**: the internal APIs of each layer, called directly through
  `kubectl port-forward`. These skip the mesh, so use them for debugging a single service.

## Tracing
The gateway starts a new trace for every request (it drops client-sent trace headers) and the
response's `traceresponse` header names it. The Postman console prints the Kibana links for each request:
- **APM**: the trace waterfall, `{{kibanaUrl}}/app/apm/link-to/trace/<traceId>`
- **Logs**: Discover filtered on `trace.id` (app logs + Envoy access logs from every hop)

The same links are kept in the `lastTraceUrl` / `lastLogsUrl` collection variables.

## CLI
`make postman` runs the Storefront folder with newman; `make postman-debug` runs the debug folders.
"""

collection = {
    "info": {
        "_postman_id": uid("collection"),
        "name": "meshshop",
        "description": DESCRIPTION,
        "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
    },
    "item": [storefront, business, catalog, inventory, order],
    "event": [script("prerequest", COLLECTION_PRE), script("test", COLLECTION_TEST)],
    "variable": [
        {"key": "baseUrl", "value": "https://meshshop.localhost:8443", "description": "Istio ingress gateway (make gateway)"},
        {"key": "kibanaUrl", "value": "http://localhost:5601", "description": "Kibana (make kibana)"},
        {"key": "userId", "value": "alice", "description": "customerId for the debug folders"},
        {"key": "aliceToken", "value": "", "description": "set by the collection pre-request script"},
        {"key": "aliceTokenExp", "value": "0"},
        {"key": "bobToken", "value": "", "description": "set by the collection pre-request script"},
        {"key": "bobTokenExp", "value": "0"},
        {"key": "checkoutKey", "value": "", "description": "Idempotency-Key of the last Checkout"},
        {"key": "shoppingUrl", "value": "http://localhost:18081", "description": "make debug-ports"},
        {"key": "catalogUrl", "value": "http://localhost:18082", "description": "make debug-ports"},
        {"key": "inventoryUrl", "value": "http://localhost:18083", "description": "make debug-ports"},
        {"key": "orderUrl", "value": "http://localhost:18084", "description": "make debug-ports"},
        {"key": "inStockProductId", "value": "p-100", "description": "set by Home"},
        {"key": "orderId", "value": "", "description": "set by Checkout"},
        {"key": "debugOrderId", "value": "", "description": "set by order debug requests"},
        {"key": "reservationId", "value": "", "description": "set by Reserve stock"},
        {"key": "stockProductId", "value": "p-104", "description": "set by Stock levels"},
        {"key": "traceId", "value": "", "description": "set per request by the pre-request script"},
        {"key": "lastTraceUrl", "value": ""},
        {"key": "lastLogsUrl", "value": ""},
    ],
}

environment = {
    "id": uid("env-local"),
    "name": "meshshop · local",
    "values": [
        {"key": k, "value": v, "type": "secret" if k.endswith("Password") else "default", "enabled": True} for k, v in [
            ("baseUrl", "https://meshshop.localhost:8443"), ("kibanaUrl", "http://localhost:5601"),
            ("userId", "alice"), ("alicePassword", ""), ("bobPassword", ""),
            ("shoppingUrl", "http://localhost:18081"), ("catalogUrl", "http://localhost:18082"),
            ("inventoryUrl", "http://localhost:18083"), ("orderUrl", "http://localhost:18084"),
        ]
    ],
    "_postman_variable_scope": "environment",
}

with open(f"{OUT}/meshshop.postman_collection.json", "w") as f:
    json.dump(collection, f, indent=2, ensure_ascii=False)
    f.write("\n")
with open(f"{OUT}/meshshop.local.postman_environment.json", "w") as f:
    json.dump(environment, f, indent=2, ensure_ascii=False)
    f.write("\n")
n = sum(len(fo["item"]) for fo in collection["item"])
print(f"{n} requests in {len(collection['item'])} folders")
