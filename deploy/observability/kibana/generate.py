#!/usr/bin/env python3
"""Generates meshshop.ndjson: Kibana saved objects (data view, Discover sessions,
Lens charts, dashboard). Edit this file and re-run rather than editing the ndjson:

    python3 deploy/observability/kibana/generate.py
"""
import json
import os

DV = "meshshop-logs"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "meshshop.ndjson")
objects = []


def add(obj):
    objects.append(obj)
    return obj["id"]


# ---- data view ---------------------------------------------------------------
add({
    "type": "index-pattern", "id": DV,
    "attributes": {"title": "logs-meshshop.*", "name": "meshshop logs", "timeFieldName": "@timestamp"},
    "references": [],
})


# ---- Discover sessions -------------------------------------------------------
def search(id_, title, query, columns):
    return add({
        "type": "search", "id": id_,
        "attributes": {
            "title": title,
            "columns": columns,
            "sort": [["@timestamp", "desc"]],
            "kibanaSavedObjectMeta": {"searchSourceJSON": json.dumps({
                "query": {"query": query, "language": "kuery"},
                "filter": [],
                "indexRefName": "kibanaSavedObjectMeta.searchSourceJSON.index",
            })},
        },
        "references": [{"name": "kibanaSavedObjectMeta.searchSourceJSON.index", "type": "index-pattern", "id": DV}],
    })


s_app = search("meshshop-app-logs", "meshshop · App logs",
               'data_stream.dataset : "meshshop.app"',
               ["service.name", "labels.layer", "log.level", "message", "trace.id"])
s_envoy = search("meshshop-envoy-logs", "meshshop · Envoy access logs",
                 'data_stream.dataset : "meshshop.envoy"',
                 ["service.name", "envoy.direction", "http.response.status_code", "message", "trace.id"])
s_rbac = search("meshshop-rbac-denials", "meshshop · RBAC denials (layer violations)",
                'data_stream.dataset : "meshshop.envoy" and envoy.rbac_denied : true',
                ["service.name", "envoy.downstream_peer", "url.original", "envoy.response_code_details"])
s_errors = search("meshshop-problems", "meshshop · Warnings and errors",
                  'log.level : ("warn" or "warning" or "error")',
                  ["service.name", "log.level", "message", "error.message", "trace.id"])


# ---- Lens charts -------------------------------------------------------------
def ts_col():
    return {"label": "@timestamp", "dataType": "date", "operationType": "date_histogram",
            "sourceField": "@timestamp", "isBucketed": True, "scale": "interval",
            "params": {"interval": "auto", "includeEmptyRows": True, "dropPartials": False}}


def terms_col(field, label, order_col, size=10):
    return {"label": label, "dataType": "string", "operationType": "terms", "sourceField": field,
            "isBucketed": True, "scale": "ordinal",
            "params": {"size": size, "orderBy": {"type": "column", "columnId": order_col},
                       "orderDirection": "desc", "otherBucket": True, "missingBucket": False}}


def count_col(label="Requests"):
    return {"label": label, "customLabel": True, "dataType": "number", "operationType": "count",
            "sourceField": "___records___", "isBucketed": False, "scale": "ratio", "params": {"emptyAsNull": True}}


def lens_xy(id_, title, query, split_field, split_label, metric, series="bar_stacked"):
    cols = {"ts": ts_col(), "split": terms_col(split_field, split_label, "m"), "m": metric}
    return add({
        "type": "lens", "id": id_,
        "attributes": {
            "title": title, "description": "", "visualizationType": "lnsXY",
            "state": {
                "datasourceStates": {"formBased": {"layers": {"l1": {
                    "columns": cols, "columnOrder": ["split", "ts", "m"], "incompleteColumns": {}}}}},
                "visualization": {
                    "legend": {"isVisible": True, "position": "right"},
                    "valueLabels": "hide", "preferredSeriesType": series,
                    "layers": [{"layerId": "l1", "layerType": "data", "seriesType": series,
                                "xAccessor": "ts", "splitAccessor": "split", "accessors": ["m"]}],
                },
                "query": {"query": query, "language": "kuery"},
                "filters": [],
            },
        },
        "references": [{"type": "index-pattern", "id": DV, "name": "indexpattern-datasource-layer-l1"}],
        # Written in the current (formBased) Lens format: skip the 7.x-era migrations.
        "typeMigrationVersion": "8.9.0", "coreMigrationVersion": "8.8.0",
    })


INBOUND = 'data_stream.dataset : "meshshop.envoy" and envoy.direction : "inbound"'

v_req = lens_xy("meshshop-lens-requests", "Requests by service (sidecar inbound)",
                INBOUND, "service.name", "Service", count_col())
v_layer = lens_xy("meshshop-lens-layers", "Requests by layer",
                  INBOUND, "labels.layer", "Layer", count_col(), series="area_stacked")
v_status = lens_xy("meshshop-lens-status", "Responses by status code",
                   INBOUND, "http.response.status_code", "Status", count_col())
v_latency = lens_xy("meshshop-lens-latency", "p95 latency by service (ms)",
                    INBOUND, "service.name", "Service",
                    {"label": "p95 duration (ms)", "customLabel": True, "dataType": "number",
                     "operationType": "percentile", "sourceField": "envoy.duration", "isBucketed": False,
                     "scale": "ratio", "params": {"percentile": 95}},
                    series="line")
v_rbac = lens_xy("meshshop-lens-rbac", "RBAC denials by caller identity",
                 'data_stream.dataset : "meshshop.envoy" and envoy.rbac_denied : true',
                 "envoy.downstream_peer", "Caller (SPIFFE id)", count_col("Denied"))
v_levels = lens_xy("meshshop-lens-levels", "App warnings and errors by service",
                   'data_stream.dataset : "meshshop.app" and log.level : ("warn" or "error")',
                   "service.name", "Service", count_col("Log lines"))


# ---- dashboard ---------------------------------------------------------------
layout = [  # (object id, type, x, y, w, h)
    (v_req, "lens", 0, 0, 24, 12), (v_layer, "lens", 24, 0, 24, 12),
    (v_status, "lens", 0, 12, 24, 12), (v_latency, "lens", 24, 12, 24, 12),
    (v_rbac, "lens", 0, 24, 24, 12), (v_levels, "lens", 24, 24, 24, 12),
    (s_errors, "search", 0, 36, 48, 15),
]
panels, refs = [], []
for i, (oid, typ, x, y, w, h) in enumerate(layout):
    pid = "p%d" % i
    panels.append({"panelIndex": pid, "gridData": {"x": x, "y": y, "w": w, "h": h, "i": pid},
                   "type": typ, "panelRefName": "panel_" + pid, "embeddableConfig": {}})
    refs.append({"name": "panel_" + pid, "type": typ, "id": oid})

add({
    "type": "dashboard", "id": "meshshop-overview",
    "attributes": {
        "title": "meshshop · Service mesh overview",
        "description": "Envoy access logs and app logs for the meshshop services. Traces: Observability > APM.",
        "panelsJSON": json.dumps(panels),
        "optionsJSON": json.dumps({"useMargins": True, "syncColors": True, "syncTooltips": True, "hidePanelTitles": False}),
        "timeRestore": True, "timeFrom": "now-1h", "timeTo": "now",
        "refreshInterval": {"pause": False, "value": 30000},
        "kibanaSavedObjectMeta": {"searchSourceJSON": json.dumps({"query": {"query": "", "language": "kuery"}, "filter": []})},
    },
    "references": refs,
})

with open(OUT, "w") as f:
    for o in objects:
        f.write(json.dumps(o, separators=(",", ":")) + "\n")
print("wrote %d objects to %s" % (len(objects), OUT))
