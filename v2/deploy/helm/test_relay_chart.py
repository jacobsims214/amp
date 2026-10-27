#!/usr/bin/env python3
"""Helm template tests for the remote control relay (epic 165, AMP #2387).

Least privilege (#2392): amp-relay connects as its own CloudNativePG managed
role (amp_relay, Secret amp-relay-db), never with amp-api's app credentials;
it and amp-api share the relay's own cluster token (Secret
amp-relay-cluster-token); amp-api names the role to grant (RELAY_DB_ROLE); and
a NetworkPolicy admits :3002 only from Envoy and :3003 only from amp-relay and
amp-api.

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


def render_error(*args):
    """helm template's error output, or None when it renders."""
    p = subprocess.run(["helm", "template", "amp", CHART, "-n", "amp", *args], capture_output=True, text=True)
    return None if p.returncode == 0 else p.stderr


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


def has_env(dep, name):
    return any(e["name"] == name for e in dep["spec"]["template"]["spec"]["containers"][0]["env"])


def env_secret(dep, name):
    """(secret name, key) an env var comes from, or None."""
    for e in dep["spec"]["template"]["spec"]["containers"][0]["env"]:
        if e["name"] == name:
            ref = e.get("valueFrom", {}).get("secretKeyRef")
            return (ref["name"], ref["key"]) if ref else None
    return None


def secret_refs(dep):
    """Every Secret a Deployment's containers read (env and envFrom)."""
    out = set()
    for c in dep["spec"]["template"]["spec"]["containers"] + dep["spec"]["template"]["spec"].get("initContainers", []):
        for e in c.get("env", []):
            ref = e.get("valueFrom", {}).get("secretKeyRef")
            if ref:
                out.add(ref["name"])
        for ef in c.get("envFrom", []):
            if "secretRef" in ef:
                out.add(ef["secretRef"]["name"])
    for v in dep["spec"]["template"]["spec"].get("volumes", []):
        if "secret" in v:
            out.add(v["secret"]["secretName"])
    return out


def test_least_privilege(docs, label, db_secret="amp-relay-db", tok_secret="amp-relay-cluster-token", role="amp_relay",
                         chart_secrets=True):
    """#2392: the relay's own DB role and cluster token; amp-api's credentials not mounted in amp-relay."""
    relay = find(docs, "Deployment", "amp-relay")[0]
    api = find(docs, "Deployment", "amp-api")[0]
    refs = secret_refs(relay)
    check("amp-postgres-app" not in refs, f"{label}: amp-relay mounts amp-api's database Secret amp-postgres-app")
    check(refs <= {db_secret, tok_secret}, f"{label}: amp-relay reads unexpected Secrets {sorted(refs - {db_secret, tok_secret})}")
    check(env_secret(relay, "PGUSER") == (db_secret, "username"), f"{label}: amp-relay PGUSER from {env_secret(relay, 'PGUSER')}")
    check(env_secret(relay, "PGPASSWORD") == (db_secret, "password"), f"{label}: amp-relay PGPASSWORD from {env_secret(relay, 'PGPASSWORD')}")
    check(env_secret(relay, "RELAY_CLUSTER_TOKEN") == (tok_secret, "token"), f"{label}: amp-relay RELAY_CLUSTER_TOKEN")
    check(env_secret(api, "RELAY_CLUSTER_TOKEN") == (tok_secret, "token"), f"{label}: amp-api RELAY_CLUSTER_TOKEN")
    check(env_secret(api, "PGUSER") == ("amp-postgres-app", "username"), f"{label}: amp-api must keep its own DB role")
    check(amp_api_env(docs, "RELAY_DB_ROLE") == role, f"{label}: amp-api RELAY_DB_ROLE {amp_api_env(docs, 'RELAY_DB_ROLE')}")
    cnpg = find(docs, "Cluster", "amp-postgres")[0]
    roles = cnpg["spec"].get("managed", {}).get("roles", [])
    check(len(roles) == 1 and roles[0]["name"] == role, f"{label}: managed roles {roles}")
    if roles:
        r = roles[0]
        check(r["login"] is True and r["ensure"] == "present", f"{label}: relay role must log in")
        # Least privilege: none of these may be granted. CNPG's default for
        # each is false, and #2474: the chart must not write them as `false`
        # either, because CNPG drops false omitempty booleans when it stores
        # the Cluster and ArgoCD's diff then never matches (endless
        # OutOfSync/self-heal). Absent is the only allowed value.
        for flag in ("superuser", "createdb", "createrole", "replication", "bypassrls"):
            check(flag not in r, f"{label}: relay role renders {flag}: {r.get(flag)!r} (omit it; CNPG's default is false)")
        check(r.get("inherit") is False, f"{label}: relay role inherit must be false")
    # #2474: no managed role renders a false flag CNPG would drop. inherit is
    # the one boolean CNPG keeps as false (a pointer defaulting to true).
    for role_ in roles:
        dropped = sorted(k for k, v in role_.items() if v is False and k != "inherit")
        check(not dropped, f"{label}: managed role {role_.get('name')} renders false flags CNPG drops: {dropped}")
    if roles:
        check(r["passwordSecret"]["name"] == db_secret, f"{label}: relay role password Secret {r['passwordSecret']}")
    sec = find(docs, "Secret", "amp-relay-db")
    tok = find(docs, "Secret", "amp-relay-cluster-token")
    if chart_secrets:
        check(len(sec) == 1 and sec[0]["type"] == "kubernetes.io/basic-auth", f"{label}: amp-relay-db must be a basic-auth Secret")
        if sec:
            check(sec[0]["stringData"]["username"] == role, f"{label}: amp-relay-db username")
            check(len(sec[0]["stringData"]["password"]) >= 32, f"{label}: amp-relay-db password too short")
        check(len(tok) == 1 and len(tok[0]["stringData"]["token"]) >= 32, f"{label}: amp-relay-cluster-token")
    else:
        check(not sec and not tok, f"{label}: existingSecret set but the chart still makes the Secrets")
    np = find(docs, "NetworkPolicy", "amp-relay")
    check(len(np) == 1, f"{label}: no amp-relay NetworkPolicy")
    if np:
        spec = np[0]["spec"]
        check(spec["podSelector"]["matchLabels"] == {"app": "amp-relay"}, f"{label}: NetworkPolicy selects {spec['podSelector']}")
        check(spec["policyTypes"] == ["Ingress"], f"{label}: NetworkPolicy types {spec['policyTypes']}")
        allowed = {}
        for rule in spec["ingress"]:
            ports = tuple(sorted(p["port"] for p in rule["ports"]))
            apps = sorted(f["podSelector"]["matchLabels"]["app"] for f in rule["from"])
            allowed[ports] = apps
        check(allowed == {(3002,): ["envoy"], (3003,): ["amp-api", "amp-relay"]}, f"{label}: NetworkPolicy ingress {allowed}")
        # The labels the policy names are the ones the pods carry.
        for app in ("envoy", "amp-api", "amp-relay"):
            d = find(docs, "Deployment", app)
            check(d and d[0]["spec"]["template"]["metadata"]["labels"].get("app") == app, f"{label}: pod label app={app}")


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
    for name in ("RELAY_DB_ROLE", "RELAY_CLUSTER_TOKEN"):
        check(not has_env(find(docs, "Deployment", "amp-api")[0], name), f"{label}: relay off still sets amp-api {name}")
    check(not find(docs, "NetworkPolicy", "amp-relay"), f"{label}: relay off renders a NetworkPolicy")
    for sec in ("amp-relay-db", "amp-relay-cluster-token"):
        check(not find(docs, "Secret", sec), f"{label}: relay off renders Secret {sec}")
    cnpg = find(docs, "Cluster", "amp-postgres")
    check(cnpg and "managed" not in cnpg[0]["spec"], f"{label}: relay off still declares a managed role")
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
    test_least_privilege(docs, label)
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
            check(h["route"].get("timeout") == "0s", f"{where}: command stream timeout")
            check(h["route"].get("max_stream_duration", {}).get("max_stream_duration") == "43200s", f"{where}: command stream max")
            check(h["route"].get("idle_timeout") == "120s", f"{where}: command stream idle timeout")
            check(ext_authz_disabled(h), f"{where}: /relay/ must skip ext_authz")
        # #2399: only the stream is unbounded; the short harness calls keep a timeout.
        for p in ("/relay/v1/sessions", "/relay/v1/sessions/rs_x/token", "/relay/v1/sessions/rs_x/events",
                  "/relay/v1/sessions/rs_x/heartbeat", "/relay/v1/sessions/rs_x/pairings", "/relay/v1/sessions/rs_x",
                  "/relay/v1/sessions/rs_x/commands/x"):
            r = first_match(rs, p)
            check(r is not None and r["route"]["cluster"] == "amp_relay", f"{where}: {p} not routed to amp_relay")
            if r:
                check(r["route"].get("timeout") == "30s", f"{where}: {p} timeout {r['route'].get('timeout')}")
                check("max_stream_duration" not in r["route"], f"{where}: {p} is not a stream")
                check(ext_authz_disabled(r), f"{where}: {p} must skip ext_authz")
        for p in ("/api/sessions", "/api/sessions/revoke-all", "/api/sessions/rs_x", "/api/sessions/rs_x/commands"):
            r = first_match(rs, p)
            check(r is not None and r["route"]["cluster"] == "amp_relay", f"{where}: {p} not routed to amp_relay")
            check(r is None or not ext_authz_disabled(r), f"{where}: {p} must keep ext_authz")
        for p in ("/api/sessions", "/api/sessions/rs_x/commands"):
            r = first_match(rs, p)
            check(r is not None and r["route"].get("timeout") == "30s", f"{where}: {p} timeout")
        r = first_match(rs, "/api/sessions/rs_x/events")
        check(r is not None and r["route"]["cluster"] == "amp_relay" and r["route"].get("timeout") == "0s",
              f"{where}: events stream route")
        check(r is not None and r["route"].get("max_stream_duration", {}).get("max_stream_duration") == "43200s"
              and r["route"].get("idle_timeout") == "120s", f"{where}: events stream bounds")
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
        # Envoy runs as a non-root user in its image: on Linux it must be able
        # to read the mounted files (TemporaryDirectory is 0700).
        for p in (d, tls):
            os.chmod(p, 0o755)
        for p in (path, os.path.join(tls, "tls.crt")):
            os.chmod(p, 0o644)
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
    prod_shipped = ["-f", os.path.join(CHART, "values-production.yaml")]
    configs["off-prod"] = test_off(prod_shipped + ["--set", "relay.enabled=false"], "production")
    configs["on"] = test_on([], "default")
    # #2470: production ships with the relay on, its password and token in
    # Secrets created by hand (the chart renders neither).
    _, docs = render(*prod_shipped)
    test_least_privilege(docs, "production as shipped", db_secret="amp-relay-db", tok_secret="amp-relay-cluster-token",
                         chart_secrets=False)
    # The cases below start from production with those pins cleared.
    prod = prod_shipped + ["--set", "relay.database.existingSecret=", "--set", "relay.clusterToken.existingSecret="]
    # #2399: production refuses the relay until its password and token are pinned.
    err = render_error(*prod, "--set", "relay.enabled=true")
    check(err is not None and "relay.database.existingSecret" in err, f"production without pinned relay secrets rendered: {err}")
    err = render_error(*prod, "--set", "relay.enabled=true", "--set", "relay.database.existingSecret=my-relay-db")
    check(err is not None and "relay.clusterToken.existingSecret" in err, f"production without a pinned token rendered: {err}")
    check(render_error(*prod, "--set", "relay.enabled=false") is None, "production with the relay off must render")
    check(render_error("--set", "relay.enabled=true") is None, "the default profile must render without pins")
    _, docs = render(*prod, "--set", "relay.enabled=true", "--set", "relay.database.existingSecret=my-relay-db",
                     "--set", "relay.clusterToken.existingSecret=my-relay-token")
    test_least_privilege(docs, "production existing secrets", db_secret="my-relay-db", tok_secret="my-relay-token",
                         chart_secrets=False)
    configs["on-prod"] = test_on(prod + ["--set", "global.secretOverrides.relayDbPassword=" + "p" * 40,
                                         "--set", "global.secretOverrides.relayClusterToken=" + "t" * 40], "production")
    configs["on-tls-off"] = test_on(["--set", "global.tls.enabled=false"], "tls-off")
    configs["on-1-replica"] = test_on(["--set", "relay.replicas=1"], "1 replica", replicas=1)
    # Secrets you manage instead of the chart's; the NetworkPolicy can be off.
    _, docs = render("--set", "relay.enabled=true", "--set", "relay.database.existingSecret=my-relay-db",
                     "--set", "relay.clusterToken.existingSecret=my-relay-token", "--set", "relay.database.role=relay_ro")
    test_least_privilege(docs, "existing secrets", db_secret="my-relay-db", tok_secret="my-relay-token", role="relay_ro",
                         chart_secrets=False)
    _, docs = render("--set", "relay.enabled=true", "--set", "relay.networkPolicy.enabled=false")
    check(not find(docs, "NetworkPolicy", "amp-relay"), "networkPolicy.enabled=false still renders it")
    # Pinned values (ArgoCD) are used as given.
    _, docs = render("--set", "relay.enabled=true", "--set", "global.secretOverrides.relayDbPassword=" + "p" * 40,
                     "--set", "global.secretOverrides.relayClusterToken=" + "t" * 40)
    sec, tok = find(docs, "Secret", "amp-relay-db"), find(docs, "Secret", "amp-relay-cluster-token")
    check(sec and sec[0]["stringData"]["password"] == "p" * 40, "secretOverrides.relayDbPassword not used")
    check(tok and tok[0]["stringData"]["token"] == "t" * 40, "secretOverrides.relayClusterToken not used")
    if "--envoy-validate" in sys.argv:
        for label, ev in configs.items():
            envoy_validate(label, ev)
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("relay chart tests: all passed")


if __name__ == "__main__":
    main()
