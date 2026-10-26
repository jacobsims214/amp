#!/usr/bin/env python3
"""Helm template tests for the audit log's client IP (AMP #2430).

KB decisions/audit-log-v2.md, section 2:
- Every Envoy listener that routes to AMP appends its TCP peer to
  X-Forwarded-For (use_remote_address: true), trusts no earlier hop
  (xff_num_trusted_hops: 0), treats no address as internal (an explicit,
  empty internal_address_config) and never keeps a client-sent request id.
- amp-api, authadmin, mcpoauth (and amp-relay when relay.enabled, #2433)
  get AMP_TRUSTED_PROXIES from
  clientIP.trustedProxies: nothing locally; the pod CIDR plus
  Cloudflare's ranges in values-production.yaml. Every entry parses as a
  CIDR or an address (the services trust nothing if one doesn't). Locally
  (docker-desktop) the list is empty: Envoy's peer through kubectl
  port-forward is Envoy's own pod address, so trusting the pod CIDR there
  would only make a forged header believable.
- NetworkPolicies admit only Envoy to the public ports of amp-api (3001,
  8000), authadmin (8090) and mcpoauth (8091), and only amp-api to amp-api's
  replica port (3003). Only networkPolicy.envoyFrom (Traefik in
  production, no pod locally) may reach Envoy. networkPolicy.enabled=false
  renders none.

Usage (needs helm, PyYAML, and the valkey subchart in deploy/helm/amp/charts,
e.g. `helm dependency build deploy/helm/amp`):

    python3 v2/deploy/helm/test_client_ip_chart.py [--envoy-validate]

--envoy-validate also runs `envoy --mode validate` (Docker image
envoyproxy/envoy:v1.31-latest) on every rendered Envoy config.
"""
import ipaddress
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
    return [d for d in yaml.safe_load_all(out) if d]


def find(docs, kind, name):
    return [d for d in docs if d.get("kind") == kind and d["metadata"]["name"] == name]


def envoy_yaml(docs):
    cm = find(docs, "ConfigMap", "amp-envoy-config")
    assert cm, "no amp-envoy-config ConfigMap"
    return cm[0]["data"]["envoy.yaml"]


def amp_hcms(cfg):
    """(listener, HttpConnectionManager) for every listener that routes to AMP."""
    out = []
    for lis in cfg["static_resources"]["listeners"]:
        for chain in lis["filter_chains"]:
            for f in chain["filters"]:
                hcm = f["typed_config"]
                if hcm.get("route_config", {}).get("name") == "amp_routes":
                    out.append((lis["name"], hcm))
    return out


def env_value(docs, deployment, name):
    dep = find(docs, "Deployment", deployment)
    assert dep, f"no Deployment {deployment}"
    for e in dep[0]["spec"]["template"]["spec"]["containers"][0].get("env", []):
        if e["name"] == name:
            return e.get("value")
    return None


def policy_rules(pol):
    """{ports tuple: sorted app labels allowed}."""
    out = {}
    for rule in pol["spec"]["ingress"]:
        ports = tuple(sorted(p["port"] for p in rule["ports"]))
        out[ports] = sorted(src["podSelector"]["matchLabels"]["app"] for src in rule["from"])
    return out


def test_envoy(docs, label):
    cfg = yaml.safe_load(envoy_yaml(docs))
    hcms = amp_hcms(cfg)
    check(hcms, f"{label}: no listener routes amp_routes")
    for name, hcm in hcms:
        check(hcm.get("use_remote_address") is True, f"{label}/{name}: use_remote_address must be true")
        check(hcm.get("xff_num_trusted_hops") == 0, f"{label}/{name}: xff_num_trusted_hops must be 0")
        check(hcm.get("skip_xff_append") is False, f"{label}/{name}: skip_xff_append must be false")
        iac = hcm.get("internal_address_config")
        check(iac == {"unix_sockets": False, "cidr_ranges": []},
              f"{label}/{name}: internal_address_config must be explicit and empty, got {iac}")
        check(hcm.get("preserve_external_request_id") is False, f"{label}/{name}: preserve_external_request_id must be false")
        check(hcm.get("generate_request_id") is True, f"{label}/{name}: generate_request_id must be true")


def test_trusted(docs, label, want, relay=False):
    deps = ["amp-api", "amp-authadmin", "amp-mcpoauth"]
    if relay:
        # amp-relay's public listener records remote.* entries (#2433).
        deps.append("amp-relay")
    for dep in deps:
        v = env_value(docs, dep, "AMP_TRUSTED_PROXIES")
        check(v is not None, f"{label}: {dep} has no AMP_TRUSTED_PROXIES")
        if v is None:
            continue
        entries = [e for e in v.replace(",", " ").split() if e]
        check(entries == want, f"{label}: {dep} AMP_TRUSTED_PROXIES = {entries}, want {want}")
        for e in entries:
            try:
                ipaddress.ip_network(e, strict=True)
            except ValueError as err:
                check(False, f"{label}: {dep} trusted entry {e!r} is not a CIDR: {err}")


def test_envoy_policy(docs, label, want_from):
    pol = find(docs, "NetworkPolicy", "envoy")
    check(pol, f"{label}: no NetworkPolicy envoy")
    if not pol:
        return
    spec = pol[0]["spec"]
    check(spec["podSelector"]["matchLabels"] == {"app": "envoy"}, f"{label}: envoy policy selects {spec['podSelector']}")
    check(spec["policyTypes"] == ["Ingress"], f"{label}: envoy policy policyTypes {spec['policyTypes']}")
    if not want_from:
        check(spec.get("ingress") == [], f"{label}: envoy policy must admit no pod, got {spec.get('ingress')}")
        return
    rules = spec.get("ingress") or []
    check(len(rules) == 1 and rules[0]["from"] == want_from
          and sorted(p["port"] for p in rules[0]["ports"]) == [8080, 8443],
          f"{label}: envoy policy ingress {rules}, want {want_from} on 8080/8443")


def test_policies(docs, label, enabled=True):
    want = {
        "amp-api": ("amp-api", {(3001, 8000): ["envoy"], (3003,): ["amp-api"]}),
        "amp-authadmin": ("amp-authadmin", {(8090,): ["envoy"]}),
        "amp-mcpoauth": ("amp-mcpoauth", {(8091,): ["envoy"]}),
    }
    pols = [d for d in docs if d.get("kind") == "NetworkPolicy" and d["metadata"]["name"] in list(want) + ["envoy"]]
    if not enabled:
        check(not pols, f"{label}: networkPolicy.enabled=false still renders {[p['metadata']['name'] for p in pols]}")
        return
    for name, (app, rules) in want.items():
        pol = find(docs, "NetworkPolicy", name)
        check(pol, f"{label}: no NetworkPolicy {name}")
        if not pol:
            continue
        spec = pol[0]["spec"]
        check(spec["podSelector"]["matchLabels"] == {"app": app}, f"{label}: {name} selects {spec['podSelector']}")
        check(spec["policyTypes"] == ["Ingress"], f"{label}: {name} policyTypes {spec['policyTypes']}")
        got = policy_rules(pol[0])
        check(got == rules, f"{label}: {name} ingress {got}, want {rules}")
    # The labels the policies rely on are the pods' real labels.
    for dep, app in (("amp-api", "amp-api"), ("amp-authadmin", "amp-authadmin"),
                     ("amp-mcpoauth", "amp-mcpoauth"), ("envoy", "envoy")):
        d = find(docs, "Deployment", dep)
        check(d and d[0]["spec"]["template"]["metadata"]["labels"].get("app") == app,
              f"{label}: Deployment {dep} pods aren't labelled app={app}")


def envoy_validate(label, ev):
    with tempfile.TemporaryDirectory(dir=os.path.dirname(os.path.abspath(__file__))) as d:
        path = os.path.join(d, "envoy.yaml")
        with open(path, "w") as f:
            f.write(ev)
        tls = os.path.join(d, "tls")
        os.mkdir(tls)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=amp.test",
                        "-keyout", os.path.join(tls, "tls.key"), "-out", os.path.join(tls, "tls.crt")],
                       check=True, capture_output=True)
        for p in (d, tls):
            os.chmod(p, 0o755)
        for p in (path, os.path.join(tls, "tls.crt"), os.path.join(tls, "tls.key")):
            os.chmod(p, 0o644)
        p = subprocess.run(
            ["docker", "run", "--rm", "-v", f"{d}:/cfg:ro", "-v", f"{tls}:/etc/envoy-tls:ro", ENVOY_IMAGE,
             "envoy", "--mode", "validate", "-c", "/cfg/envoy.yaml"],
            capture_output=True, text=True,
        )
        ok = p.returncode == 0 and "OK" in p.stdout
        check(ok, f"{label}: envoy --mode validate failed: {p.stdout[-400:]} {p.stderr[-400:]}")
        print(f"envoy validate {label}: {'OK' if ok else 'FAILED'}")


def prod_values():
    with open(os.path.join(CHART, "values-production.yaml")) as f:
        return yaml.safe_load(f)


def main():
    pv = prod_values()
    prod = pv["clientIP"]["trustedProxies"]
    traefik = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "traefik"}},
                "podSelector": {"matchLabels": {"app.kubernetes.io/name": "traefik"}}}]
    check(pv["networkPolicy"]["envoyFrom"] == traefik, "production: only Traefik may reach Envoy")
    check(prod[0] == "10.244.0.0/16", "production: the pod CIDR comes first")
    check(len([c for c in prod if ":" not in c]) == 16 and len([c for c in prod if ":" in c]) == 7,
          f"production: want the pod CIDR + 15 Cloudflare v4 + 7 v6 ranges, got {len(prod)}")
    cases = [
        ("local (tls on)", [], [], []),
        ("local (tls off)", ["--set", "global.tls.enabled=false"], [], []),
        ("local, walk on", ["--set", "clientIP.trustedProxies={10.244.0.0/16}"], ["10.244.0.0/16"], []),
        ("production", ["-f", os.path.join(CHART, "values-production.yaml")], prod, traefik),
    ]
    rendered = {}
    for label, args, want, envoy_from in cases:
        docs = render(*args)
        rendered[label] = envoy_yaml(docs)
        test_envoy(docs, label)
        test_trusted(docs, label, want)
        test_policies(docs, label)
        test_envoy_policy(docs, label, envoy_from)
    relay_on = ["--set", "relay.enabled=true"]
    relay_prod = ["-f", os.path.join(CHART, "values-production.yaml"), *relay_on,
                  "--set", "relay.database.existingSecret=relay-db", "--set", "relay.clusterToken.existingSecret=relay-token"]
    for label, args, want in (("local, relay on", relay_on, []), ("production, relay on", relay_prod, prod)):
        test_trusted(render(*args), label, want, relay=True)
    docs = render("--set", "networkPolicy.enabled=false")
    test_policies(docs, "policies off", enabled=False)
    if "--envoy-validate" in sys.argv:
        for label, ev in rendered.items():
            envoy_validate(label, ev)
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("client IP chart tests: all passed")


if __name__ == "__main__":
    main()
