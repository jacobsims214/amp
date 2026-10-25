#!/usr/bin/env python3
"""Helm template tests for the remote control relay (epic 165, AMP #2387).

relay.enabled=false must render nothing relay-related: no amp-relay
Deployment, Service or PodDisruptionBudget, no Envoy amp_relay cluster, and
no /relay/ or /api/sessions route (amp-api serves neither, so those paths
reach no relay). relay.enabled=true must render amp-relay and route /relay/
and /api/sessions/* to it, with the stream timeouts #2378 set.

Usage (from anywhere; needs helm, PyYAML, and the valkey subchart in
deploy/helm/amp/charts, e.g. `helm dependency build deploy/helm/amp`):

    python3 v2/deploy/helm/test_relay_chart.py [--envoy-validate]

--envoy-validate also runs `envoy --mode validate` (Docker image
envoyproxy/envoy:v1.31-latest) on every rendered Envoy config.
"""
import os
import subprocess
import sys
import tempfile

import yaml

CHART = os.path.join(os.path.dirname(os.path.abspath(__file__)), "amp")
ENVOY_IMAGE = os.environ.get("ENVOY_IMAGE", "envoyproxy/envoy:v1.31-latest")

failures = []


def check(cond, msg):
    if not cond:
        failures.append(msg)
        print("FAIL:", msg)


def render(*args):
    out = subprocess.run(
        ["helm", "template", "amp", CHART, "-n", "amp", *args],
        check=True, capture_output=True, text=True,
    ).stdout
    return out, [d for d in yaml.safe_load_all(out) if d]


def find(docs, kind, name):
    return [d for d in docs if d.get("kind") == kind and d["metadata"]["name"] == name]


def envoy_config(docs):
    cm = find(docs, "ConfigMap", "amp-envoy-config")
    assert cm, "no amp-envoy-config ConfigMap"
    return cm[0]["data"]["envoy.yaml"]


def amp_routes(envoy_yaml):
    """Every route of the "amp" virtual host on every listener."""
    cfg = yaml.safe_load(envoy_yaml)
    routes = []
    for l in cfg["static_resources"]["listeners"]:
        for fc in l.get("filter_chains", []):
            for f in fc.get("filters", []):
                rc = f.get("typed_config", {}).get("route_config")
                if not rc:
                    continue
                for vh in rc.get("virtual_hosts", []):
                    if vh["name"] == "amp":
                        routes.append((l["name"], vh["routes"]))
    return cfg, routes


def amp_api_env(docs, name):
    dep = find(docs, "Deployment", "amp-api")[0]
    for e in dep["spec"]["template"]["spec"]["containers"][0]["env"]:
        if e["name"] == name:
            return e.get("value")
    return None


def first_match(routes, path):
    """The route Envoy picks for path (prefix, path and safe_regex matches)."""
    import re
    for r in routes:
        m = r["match"]
        if "path" in m and m["path"] == path:
            return r
        if "prefix" in m and path.startswith(m["prefix"]):
            return r
        if "safe_regex" in m and re.fullmatch(m["safe_regex"]["regex"], path):
            return r
    return None


def ext_authz_disabled(r):
    return r.get("typed_per_filter_config", {}).get("envoy.filters.http.ext_authz", {}).get("disabled") is True


def test_off(args, label):
    raw, docs = render(*args)
    check("amp-relay" not in raw, f"{label}: relay off still renders 'amp-relay'")
    check("amp_relay" not in raw, f"{label}: relay off still renders an amp_relay Envoy cluster")
    ev = envoy_config(docs)
    check("/relay" not in ev, f"{label}: relay off still has a /relay route")
    check("/api/sessions" not in ev, f"{label}: relay off still has an /api/sessions route")
    for kind in ("Deployment", "Service", "PodDisruptionBudget"):
        check(not find(docs, kind, "amp-relay"), f"{label}: relay off renders {kind} amp-relay")
    check(amp_api_env(docs, "RELAY_ENABLED") == "false", f"{label}: amp-api RELAY_ENABLED should be \"false\"")
    _, routes = amp_routes(ev)
    for listener, rs in routes:
        for p in ("/relay/v1/sessions", "/api/sessions", "/api/sessions/rs_x/events"):
            r = first_match(rs, p)
            check(r is None or r["route"].get("cluster") != "amp_relay", f"{label}/{listener}: {p} goes to a relay")
    return ev


def test_on(args, label, replicas=2):
    raw, docs = render(*args, "--set", "relay.enabled=true")
    dep = find(docs, "Deployment", "amp-relay")
    check(len(dep) == 1, f"{label}: no amp-relay Deployment")
    check(len(find(docs, "Service", "amp-relay")) == 1, f"{label}: no amp-relay Service")
    check(len(find(docs, "PodDisruptionBudget", "amp-relay")) == (1 if replicas > 1 else 0), f"{label}: amp-relay PDB")
    if dep:
        spec = dep[0]["spec"]
        check(spec["replicas"] == replicas, f"{label}: amp-relay replicas {spec['replicas']}")
        c = spec["template"]["spec"]["containers"][0]
        check("amp-relay" in c["image"], f"{label}: amp-relay image {c['image']}")
        env = {e["name"]: e.get("value") for e in c["env"]}
        for k in ("DATABASE_URL", "OIDC_ISSUER_URL", "MCP_AUDIENCE", "REST_AUDIENCES", "CLUSTER_ADDR", "CLUSTER_ADVERTISE_ADDR", "RELAY_ADDR"):
            check(k in env, f"{label}: amp-relay env {k} missing")
        check("RELAY_ENABLED" not in env, f"{label}: amp-relay doesn't need RELAY_ENABLED")
    check(amp_api_env(docs, "RELAY_ENABLED") == "true", f"{label}: amp-api RELAY_ENABLED should be \"true\" (capability)")
    ev = envoy_config(docs)
    cfg, routes = amp_routes(ev)
    clusters = {c["name"]: c for c in cfg["static_resources"]["clusters"]}
    check("amp_relay" in clusters, f"{label}: no amp_relay cluster")
    if "amp_relay" in clusters:
        addr = clusters["amp_relay"]["load_assignment"]["endpoints"][0]["lb_endpoints"][0]["endpoint"]["address"]["socket_address"]
        check(addr["address"].startswith("amp-relay.") and addr["port_value"] == 3002, f"{label}: amp_relay upstream {addr}")
    check(len(routes) >= 1, f"{label}: no amp virtual host")
    for listener, rs in routes:
        where = f"{label}/{listener}"
        h = first_match(rs, "/relay/v1/sessions/rs_x/commands")
        check(h is not None and h["route"]["cluster"] == "amp_relay", f"{where}: /relay/ not routed to amp_relay")
        if h:
            check(h["route"].get("timeout") == "0s", f"{where}: /relay/ timeout")
            check(h["route"].get("max_stream_duration", {}).get("max_stream_duration") == "43200s", f"{where}: /relay/ max stream")
            check(ext_authz_disabled(h), f"{where}: /relay/ must skip ext_authz")
        for p in ("/api/sessions", "/api/sessions/revoke-all", "/api/sessions/rs_x", "/api/sessions/rs_x/commands"):
            r = first_match(rs, p)
            check(r is not None and r["route"]["cluster"] == "amp_relay", f"{where}: {p} not routed to amp_relay")
            check(r is None or not ext_authz_disabled(r), f"{where}: {p} must keep ext_authz")
        r = first_match(rs, "/api/sessions/rs_x/events")
        check(r is not None and r["route"]["cluster"] == "amp_relay" and r["route"].get("timeout") == "0s",
              f"{where}: events stream route")
        check(r is None or not ext_authz_disabled(r), f"{where}: events stream must keep ext_authz")
        r = first_match(rs, "/api/me")
        check(r is not None and r["route"]["cluster"] == "amp_api_rest", f"{where}: /api/me must stay on amp-api")
        r = first_match(rs, "/api/orgs/1/remote-control")
        check(r is not None and r["route"]["cluster"] == "amp_api_rest", f"{where}: the org setting must stay on amp-api")
    return ev


def envoy_validate(label, envoy_yaml):
    with tempfile.TemporaryDirectory(dir=os.path.dirname(os.path.abspath(__file__))) as d:
        path = os.path.join(d, "envoy.yaml")
        with open(path, "w") as f:
            f.write(envoy_yaml)
        # The TLS listener loads /etc/envoy-tls (cert-manager's secret in the
        # cluster): a throwaway self-signed pair stands in for it.
        tls = os.path.join(d, "tls")
        os.mkdir(tls)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=amp.test",
                        "-keyout", os.path.join(tls, "tls.key"), "-out", os.path.join(tls, "tls.crt")],
                       check=True, capture_output=True)
        os.chmod(os.path.join(tls, "tls.key"), 0o644)
        p = subprocess.run(
            ["docker", "run", "--rm", "-v", f"{d}:/cfg:ro", "-v", f"{tls}:/etc/envoy-tls:ro", ENVOY_IMAGE,
             "envoy", "--mode", "validate", "-c", "/cfg/envoy.yaml"],
            capture_output=True, text=True,
        )
        ok = p.returncode == 0 and "OK" in p.stdout
        check(ok, f"{label}: envoy --mode validate failed: {p.stdout[-400:]} {p.stderr[-400:]}")
        print(f"envoy validate {label}: {'OK' if ok else 'FAILED'}")


def main():
    configs = {}
    configs["off"] = test_off([], "default")
    configs["off-prod"] = test_off(["-f", os.path.join(CHART, "values-production.yaml")], "production")
    configs["on"] = test_on([], "default")
    configs["on-prod"] = test_on(["-f", os.path.join(CHART, "values-production.yaml")], "production")
    configs["on-tls-off"] = test_on(["--set", "global.tls.enabled=false"], "tls-off")
    configs["on-1-replica"] = test_on(["--set", "relay.replicas=1"], "1 replica", replicas=1)
    if "--envoy-validate" in sys.argv:
        for label, ev in configs.items():
            envoy_validate(label, ev)
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("relay chart tests: all passed")


if __name__ == "__main__":
    main()
