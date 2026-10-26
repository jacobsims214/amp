#!/usr/bin/env python3
"""Helm template tests for the audit log's client IP (AMP #2430, #2469).

KB decisions/audit-log-v2.md. AMP's boundary is its own Envoy; whatever sits
in front of it belongs to the deployment and is described only by values:
- Every Envoy listener that routes to AMP decides the client from
  envoy.clientIP: xffNumTrustedHops (default 0, use_remote_address) or
  trustedCIDRs (Envoy's xff original-IP detection), never both. It treats no
  address as internal and never keeps a client-sent request id (hop mode).
- Its amp_routes route config always overwrites X-Amp-Client-Ip with the
  address Envoy decided (%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%).
- amp-api, authadmin, mcpoauth (and amp-relay when relay.enabled) get
  AMP_TRUSTED_PROXIES from clientIP.trustedProxies: empty by default, the
  pod CIDR in values-production.yaml. Every entry parses as a CIDR.
- NetworkPolicies admit only Envoy to the public ports of amp-api (3001,
  8000), authadmin (8090) and mcpoauth (8091), and only amp-api to amp-api's
  replica port (3003). The Envoy policy renders only with
  networkPolicy.envoyIngress.enabled, with the peers from values.
  networkPolicy.enabled=false renders none.
- Nothing in the chart names an ingress controller, CDN or mesh vendor.

Usage (needs helm, PyYAML, and the valkey subchart in deploy/helm/amp/charts,
e.g. `helm dependency build deploy/helm/amp`):

    python3 v2/deploy/helm/test_client_ip_chart.py [--envoy-validate] [--envoy-run]

--envoy-validate also runs `envoy --mode validate` (Docker image
envoyproxy/envoy:v1.31-latest) on every rendered Envoy config.
--envoy-run starts the rendered Envoy in Docker in front of an echo upstream
and checks which client address reaches the services, including spoofed
X-Forwarded-For and X-Amp-Client-Ip headers.
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


def test_envoy(docs, label, hops=0, cidrs=None):
    cfg = yaml.safe_load(envoy_yaml(docs))
    hcms = amp_hcms(cfg)
    check(hcms, f"{label}: no listener routes amp_routes")
    for name, hcm in hcms:
        if cidrs:
            check(hcm.get("use_remote_address") is False, f"{label}/{name}: CIDR mode needs use_remote_address false")
            check("xff_num_trusted_hops" not in hcm, f"{label}/{name}: CIDR mode can't set xff_num_trusted_hops")
            ext = hcm.get("original_ip_detection_extensions") or []
            got = None
            if len(ext) == 1 and ext[0]["name"] == "envoy.extensions.http.original_ip_detection.xff":
                got = [f"{c['address_prefix']}/{c['prefix_len']}" for c in ext[0]["typed_config"]["xff_trusted_cidrs"]["cidrs"]]
            check(got == cidrs, f"{label}/{name}: xff_trusted_cidrs = {got}, want {cidrs}")
        else:
            check(hcm.get("use_remote_address") is True, f"{label}/{name}: use_remote_address must be true")
            check(hcm.get("xff_num_trusted_hops") == hops, f"{label}/{name}: xff_num_trusted_hops must be {hops}")
            check("original_ip_detection_extensions" not in hcm, f"{label}/{name}: hop mode has no detection extension")
        check(hcm.get("skip_xff_append") is False, f"{label}/{name}: skip_xff_append must be false")
        iac = hcm.get("internal_address_config")
        check(iac == {"unix_sockets": False, "cidr_ranges": []},
              f"{label}/{name}: internal_address_config must be explicit and empty, got {iac}")
        check(hcm.get("preserve_external_request_id") is False, f"{label}/{name}: preserve_external_request_id must be false")
        check(hcm.get("generate_request_id") is True, f"{label}/{name}: generate_request_id must be true")
        add = hcm["route_config"].get("request_headers_to_add")
        check(add == [{"header": {"key": "x-amp-client-ip", "value": "%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%"},
                       "append_action": "OVERWRITE_IF_EXISTS_OR_ADD"}],
              f"{label}/{name}: amp_routes must overwrite x-amp-client-ip with Envoy's decision, got {add}")


def render_fails(*args):
    p = subprocess.run(["helm", "template", "amp", CHART, "-n", "amp", *args], capture_output=True, text=True)
    return p.returncode != 0


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
    """want_from None: no Envoy policy; [] : one that admits nobody."""
    pol = find(docs, "NetworkPolicy", "envoy")
    if want_from is None:
        check(not pol, f"{label}: an Envoy NetworkPolicy rendered without networkPolicy.envoyIngress.enabled")
        return
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


def envoy_image(docs):
    dep = find(docs, "Deployment", "envoy")
    return dep[0]["spec"]["template"]["spec"]["containers"][0]["image"]


def envoy_validate(label, ev, image=ENVOY_IMAGE):
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
            ["docker", "run", "--rm", "-v", f"{d}:/cfg:ro", "-v", f"{tls}:/etc/envoy-tls:ro", image,
             "envoy", "--mode", "validate", "-c", "/cfg/envoy.yaml"],
            capture_output=True, text=True,
        )
        ok = p.returncode == 0 and "OK" in p.stdout
        check(ok, f"{label}: envoy --mode validate failed: {p.stdout[-400:]} {p.stderr[-400:]}")
        print(f"envoy validate {label} ({image}): {'OK' if ok else 'FAILED'}")


def prod_values():
    with open(os.path.join(CHART, "values-production.yaml")) as f:
        return yaml.safe_load(f)


VENDORS = ("cloudflare", "traefik", "istio", "cilium", "calico", "nginx", "alb", "aws", "gke", "azure")


def test_no_vendor_assumptions():
    """Templates may mention a vendor only in a comment that is an example."""
    tdir = os.path.join(CHART, "templates")
    for fn in sorted(os.listdir(tdir)):
        if fn == "cert-issuers.yaml":
            continue  # its comment names cert-manager DNS-01 provider examples
        with open(os.path.join(tdir, fn)) as f:
            for i, line in enumerate(f, 1):
                low = line.lower()
                for v in ("cloudflare", "traefik"):
                    check(v not in low, f"templates/{fn}:{i} mentions {v}: {line.strip()}")


# The echo upstream for --envoy-run: answers every request with the
# headers it got, as JSON. Stands in for every cluster the config routes to.
ECHO = """
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
class H(BaseHTTPRequestHandler):
    def _h(self):
        b = json.dumps({k.lower(): v for k, v in self.headers.items()}).encode()
        self.send_response(200); self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(b))); self.end_headers(); self.wfile.write(b)
    do_GET = do_POST = do_HEAD = _h
    def log_message(self, *a): pass
ThreadingHTTPServer(("0.0.0.0", int(__import__("sys").argv[1])), H).serve_forever()
"""

CLIENT = """
import json, sys, urllib.request
req = urllib.request.Request("http://envoy:8080/mcp", headers=json.loads(sys.argv[1]))
print(urllib.request.urlopen(req, timeout=10).read().decode())
"""

# envoy.clientIP.trustedCIDRs needs Envoy 1.32+ (the chart's default tag is
# v1.31-latest), so the CIDR cases render with this tag.
CIDR_TAG = ["--set", "envoy.tag=" + os.environ.get("ENVOY_CIDR_TAG", "v1.32-latest")]

PY_IMAGE = os.environ.get("PY_IMAGE", "python:3.12-alpine")


def docker(*args, check_rc=True):
    p = subprocess.run(["docker", *args], capture_output=True, text=True)
    if check_rc and p.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args[:3])}: {p.stderr[-400:]}")
    return p.stdout.strip()


def envoy_run():
    """Rendered Envoy (tls off) in front of an echo upstream, in Docker."""
    import json
    import time
    tag = f"amp-ip-{os.getpid()}"
    net = tag
    hosts = ["ui", "amp-api", "amp-authadmin", "amp-asynqmon", "amp-mcpoauth", "dex", "oauth2-proxy"]
    ports = sorted({80, 3001, 8000, 8090, 8091, 8092, 4180, 5556})
    containers = []
    with tempfile.TemporaryDirectory(dir=os.path.dirname(os.path.abspath(__file__))) as d:
        os.chmod(d, 0o755)
        with open(os.path.join(d, "echo.py"), "w") as f:
            f.write(ECHO)
        with open(os.path.join(d, "client.py"), "w") as f:
            f.write(CLIENT)
        for fn in ("echo.py", "client.py"):
            os.chmod(os.path.join(d, fn), 0o644)
        try:
            docker("network", "create", net)
            subnet = json.loads(docker("network", "inspect", net))[0]["IPAM"]["Config"][0]["Subnet"]
            for port in ports:
                name = f"{tag}-echo-{port}"
                aliases = []
                for h in hosts:
                    aliases += ["--network-alias", f"{h}.amp.svc.cluster.local"]
                docker("run", "-d", "--rm", "--name", name, "--network", net, *aliases,
                       "-v", f"{d}:/t:ro", PY_IMAGE, "python", "/t/echo.py", str(port))
                containers.append(name)

            def run_case(label, args, sends, want):
                """sends: request headers; want: X-Amp-Client-Ip at the service, or 'CLIENT' for
                the client container's own address."""
                docs = render("--set", "global.tls.enabled=false", *args)
                ev = envoy_yaml(docs)
                cfg_path = os.path.join(d, "envoy.yaml")
                with open(cfg_path, "w") as f:
                    f.write(ev)
                os.chmod(cfg_path, 0o644)
                name = f"{tag}-envoy"
                docker("rm", "-f", name, check_rc=False)
                docker("run", "-d", "--rm", "--name", name, "--network", net, "--network-alias", "envoy",
                       "-v", f"{d}:/cfg:ro", envoy_image(docs), "envoy", "-c", "/cfg/envoy.yaml", "--log-level", "warn")
                containers.append(name)
                out = None
                for _ in range(40):
                    p = subprocess.run(["docker", "run", "--rm", "--network", net, "-v", f"{d}:/t:ro", PY_IMAGE,
                                        "sh", "-c", f"hostname -i; python /t/client.py '{json.dumps(sends)}'"],
                                       capture_output=True, text=True)
                    if p.returncode == 0:
                        lines = p.stdout.strip().splitlines()
                        out = (lines[0].split()[0], json.loads(lines[-1]))
                        break
                    time.sleep(0.5)
                docker("rm", "-f", name, check_rc=False)
                if out is None:
                    check(False, f"envoy-run {label}: no answer through Envoy")
                    return
                client_ip, seen = out
                exp = client_ip if want == "CLIENT" else want
                got = seen.get("x-amp-client-ip")
                check(got == exp, f"envoy-run {label}: X-Amp-Client-Ip = {got!r}, want {exp!r} (xff {seen.get('x-forwarded-for')!r})")
                print(f"envoy-run {label}: {'OK' if got == exp else 'FAILED'} (client {client_ip}, X-Amp-Client-Ip {got}, XFF {seen.get('x-forwarded-for')})")

            spoof = {"X-Forwarded-For": "6.6.6.6, 203.0.113.7", "X-Amp-Client-Ip": "6.6.6.6"}
            run_case("hops 0, no headers", [], {}, "CLIENT")
            run_case("hops 0 ignores a spoofed XFF and X-Amp-Client-Ip", [], spoof, "CLIENT")
            run_case("hops 1 takes the rightmost XFF entry", ["--set", "envoy.clientIP.xffNumTrustedHops=1"], spoof, "203.0.113.7")
            run_case("hops 1 without XFF: the peer", ["--set", "envoy.clientIP.xffNumTrustedHops=1"], {}, "CLIENT")
            run_case("hops 2 takes the second from the right", ["--set", "envoy.clientIP.xffNumTrustedHops=2"],
                     {"X-Forwarded-For": "6.6.6.6, 203.0.113.7, 198.51.100.1"}, "203.0.113.7")
            # CIDR mode. The client container plays the trusted proxy (or a mesh
            # sidecar, which is Envoy's peer at 127.0.0.6 under Istio).
            run_case("CIDRs: untrusted peer ignores XFF", [*CIDR_TAG, "--set", "envoy.clientIP.trustedCIDRs={192.0.2.0/24}"],
                     spoof, "CLIENT")
            run_case("CIDRs: trusted peer, first untrusted entry from the right",
                     [*CIDR_TAG, "--set", f"envoy.clientIP.trustedCIDRs={{{subnet}}}"],
                     {"X-Forwarded-For": "6.6.6.6, 203.0.113.7, 198.51.100.1", "X-Amp-Client-Ip": "6.6.6.6"}, "198.51.100.1")
            run_case("CIDRs: trusted entries are skipped",
                     [*CIDR_TAG, "--set", f"envoy.clientIP.trustedCIDRs={{{subnet},198.51.100.0/24}}"],
                     {"X-Forwarded-For": "6.6.6.6, 203.0.113.7, 198.51.100.1"}, "203.0.113.7")
            run_case("CIDRs: trusted peer without XFF: the peer", [*CIDR_TAG, "--set", f"envoy.clientIP.trustedCIDRs={{{subnet}}}"],
                     {}, "CLIENT")
        finally:
            for c in containers:
                docker("rm", "-f", c, check_rc=False)
            docker("network", "rm", net, check_rc=False)


def main():
    pv = prod_values()
    prod = pv["clientIP"]["trustedProxies"]
    check(prod == ["10.244.0.0/16"], f"production: services trust Envoy's pod CIDR only, got {prod}")
    check("clientIP" not in (pv.get("envoy") or {}), "production: envoy.clientIP stays at the chart default")
    check(pv["networkPolicy"].get("envoyIngress", {}).get("enabled") is False, "production: no Envoy ingress policy")
    test_no_vendor_assumptions()
    peers = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ingress"}},
              "podSelector": {"matchLabels": {"app.kubernetes.io/name": "my-ingress"}}},
             {"ipBlock": {"cidr": "192.0.2.0/24"}}]
    peer_args = ["--set", "networkPolicy.envoyIngress.enabled=true",
                 "--set-json", f"networkPolicy.envoyIngress.from={__import__('json').dumps(peers)}"]
    cases = [
        # label, args, trusted, envoy policy peers, hops, cidrs
        ("local (tls on)", [], [], None, 0, None),
        ("local (tls off)", ["--set", "global.tls.enabled=false"], [], None, 0, None),
        ("services trust a pod CIDR", ["--set", "clientIP.trustedProxies={10.244.0.0/16}"], ["10.244.0.0/16"], None, 0, None),
        ("hops 2", ["--set", "envoy.clientIP.xffNumTrustedHops=2"], [], None, 2, None),
        ("mesh: CIDRs", [*CIDR_TAG, "--set", "envoy.clientIP.trustedCIDRs={127.0.0.6/32,10.0.0.0/8,fd00::/8,192.0.2.7}",
                         "--set", "clientIP.trustedProxies={10.244.0.0/16,127.0.0.6}"],
         ["10.244.0.0/16", "127.0.0.6"], None, 0, ["127.0.0.6/32", "10.0.0.0/8", "fd00::/8", "192.0.2.7/32"]),
        ("mesh: CIDRs, tls off", [*CIDR_TAG, "--set", "global.tls.enabled=false", "--set", "envoy.clientIP.trustedCIDRs={127.0.0.6/32}"],
         [], None, 0, ["127.0.0.6/32"]),
        ("envoy policy with peers", peer_args, [], peers, 0, None),
        ("envoy policy admitting nobody", ["--set", "networkPolicy.envoyIngress.enabled=true"], [], [], 0, None),
        ("production", ["-f", os.path.join(CHART, "values-production.yaml")], prod, None, 0, None),
    ]
    rendered = {}
    for label, args, want, envoy_from, hops, cidrs in cases:
        docs = render(*args)
        rendered[label] = (envoy_yaml(docs), envoy_image(docs))
        test_envoy(docs, label, hops, cidrs)
        test_trusted(docs, label, want)
        test_policies(docs, label)
        test_envoy_policy(docs, label, envoy_from)
    check(render_fails(*CIDR_TAG, "--set", "envoy.clientIP.xffNumTrustedHops=1", "--set", "envoy.clientIP.trustedCIDRs={10.0.0.0/8}"),
          "hops and CIDRs together must fail to render")
    check(render_fails("--set", "envoy.clientIP.trustedCIDRs={10.0.0.0/8}"),
          "CIDRs with the default Envoy 1.31 tag must fail to render (no xff_trusted_cidrs before 1.32)")
    check(render_fails("--set", "envoy.clientIP.xffNumTrustedHops=-1"), "negative hops must fail to render")
    relay_on = ["--set", "relay.enabled=true"]
    relay_prod = ["-f", os.path.join(CHART, "values-production.yaml"), *relay_on,
                  "--set", "relay.database.existingSecret=relay-db", "--set", "relay.clusterToken.existingSecret=relay-token"]
    for label, args, want in (("local, relay on", relay_on, []), ("production, relay on", relay_prod, prod)):
        docs = render(*args)
        test_trusted(docs, label, want, relay=True)
        test_envoy(docs, label)
    docs = render("--set", "networkPolicy.enabled=false", "--set", "networkPolicy.envoyIngress.enabled=true")
    test_policies(docs, "policies off", enabled=False)
    if "--envoy-validate" in sys.argv:
        for label, (ev, image) in rendered.items():
            envoy_validate(label, ev, image)
    if "--envoy-run" in sys.argv:
        envoy_run()
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("client IP chart tests: all passed")


if __name__ == "__main__":
    main()
