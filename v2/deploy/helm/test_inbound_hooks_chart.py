#!/usr/bin/env python3
"""Helm template tests for inbound webhooks (/api/hooks/, AMP #2479).

GitHub's merge webhook (POST /api/hooks/github/{project_id}) has no AMP
session. envoy.inboundHooks.enabled (default true) gives /api/hooks/ its own
Envoy route to amp-api with ext_authz off, matched before the /api prefix,
on whichever listener serves the app (TLS or plain). Every other /api path
stays behind oauth2-proxy. With it off, /api/hooks/ falls to the /api route
like any other API path. Nothing outside Envoy is rendered for it: AMP's
boundary is its Envoy.

Usage (needs helm, PyYAML, and the valkey subchart in deploy/helm/amp/charts):

    python3 v2/deploy/helm/test_inbound_hooks_chart.py [--envoy-validate]
"""
import sys

from test_relay_chart import (amp_routes, check, envoy_config, envoy_validate, failures, first_match, render)


def authz_disabled(route):
    cfg = route.get("typed_per_filter_config", {}).get("envoy.filters.http.ext_authz", {})
    return cfg.get("disabled") is True


def test(args, label, enabled=True):
    _, docs = render(*args)
    ev = envoy_config(docs)
    _, listeners = amp_routes(ev)
    check(listeners, f"{label}: no amp virtual host")
    for listener, routes in listeners:
        hook = first_match(routes, "/api/hooks/github/12")
        api = first_match(routes, "/api/projects/12/github-hook/secret")
        check(hook is not None and hook.get("route", {}).get("cluster") == "amp_api_rest",
              f"{label}/{listener}: /api/hooks/github/12 doesn't reach amp-api: {hook}")
        check(api is not None and not authz_disabled(api), f"{label}/{listener}: /api lost its session check: {api}")
        if enabled:
            check(hook.get("match") == {"prefix": "/api/hooks/"}, f"{label}/{listener}: hook route is {hook.get('match')}")
            check(authz_disabled(hook), f"{label}/{listener}: /api/hooks/ still goes through ext_authz")
            check(hook["route"].get("timeout") == "15s", f"{label}/{listener}: hook timeout {hook['route'].get('timeout')}")
        else:
            check(not authz_disabled(hook) and hook.get("match") == {"prefix": "/api"},
                  f"{label}/{listener}: inboundHooks off still bypasses the session: {hook}")
        # Only the hook prefix is opened: nothing else under /api skips ext_authz.
        for r in routes:
            m = r.get("match", {})
            p = m.get("prefix", m.get("path", ""))
            if p.startswith("/api") and authz_disabled(r):
                check(p == "/api/hooks/", f"{label}/{listener}: {p} skips ext_authz")
    # Nothing in front of Envoy: no Ingress or Gateway objects for it.
    kinds = {d.get("kind") for d in docs}
    check(not kinds & {"Ingress", "HTTPRoute", "Gateway", "IngressRoute", "VirtualService"},
          f"{label}: renders ingress objects: {kinds & {'Ingress', 'HTTPRoute', 'Gateway', 'IngressRoute', 'VirtualService'}}")
    return ev


def main():
    configs = {
        "default": test([], "default"),
        "tls-off": test(["--set", "global.tls.enabled=false"], "tls-off"),
        "relay-on": test(["--set", "relay.enabled=true"], "relay-on"),
        "timeout": None,
        "off": test(["--set", "envoy.inboundHooks.enabled=false"], "off", enabled=False),
    }
    _, docs = render("--set", "envoy.inboundHooks.timeout=30s")
    _, listeners = amp_routes(envoy_config(docs))
    for listener, routes in listeners:
        hook = first_match(routes, "/api/hooks/github/1")
        check(hook["route"].get("timeout") == "30s", f"timeout value not used on {listener}: {hook}")
    configs["timeout"] = envoy_config(docs)
    if "--envoy-validate" in sys.argv:
        for label, ev in configs.items():
            envoy_validate(label, ev)
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("inbound hooks chart tests: all passed")


if __name__ == "__main__":
    main()
