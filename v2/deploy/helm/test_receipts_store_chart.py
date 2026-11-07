#!/usr/bin/env python3
"""Helm template tests for where receipt evidence lives (#126, #144).

Default: the local store on the amp-receipts PVC (RECEIPT_DIR), no SeaweedFS
objects, and more than one amp-api replica needs ReadWriteMany.
seaweedfs.enabled: a StatefulSet + ClusterIP Service (S3 port only) + key
Secret + NetworkPolicy (amp-api only) for SeaweedFS, amp-api on
RECEIPT_STORE=s3 with keys from that Secret, no amp-receipts PVC, and any
number of replicas without ReadWriteMany. ampApi.receipts.s3: the same env
for an S3 store elsewhere, keys from its existingSecret, no SeaweedFS.
SeaweedFS is never routed by Envoy.

Usage (needs helm, PyYAML, and the valkey subchart in deploy/helm/amp/charts):

    python3 v2/deploy/helm/test_receipts_store_chart.py
"""
import os
import sys

from test_relay_chart import check, envoy_config, failures, find, render, render_error

PROD = os.path.join(os.path.dirname(os.path.abspath(__file__)), "amp", "values-production.yaml")


def api_pod(docs):
    return find(docs, "Deployment", "amp-api")[0]["spec"]["template"]["spec"]


def env(docs):
    return {e["name"]: e for e in api_pod(docs)["containers"][0]["env"]}


def seaweedfs_objects(docs):
    return sorted((d["kind"], d["metadata"]["name"]) for d in docs if "seaweedfs" in d["metadata"]["name"])


def test_local(label, *args, rwx=False):
    _, docs = render(*args)
    e = env(docs)
    check(e.get("RECEIPT_DIR", {}).get("value") == "/data/receipts", f"{label}: RECEIPT_DIR {e.get('RECEIPT_DIR')}")
    check("RECEIPT_STORE" not in e and not any(k.startswith("RECEIPT_S3_") for k in e), f"{label}: s3 env on the local store")
    pvc = find(docs, "PersistentVolumeClaim", "amp-receipts")
    check(len(pvc) == 1, f"{label}: no amp-receipts PVC")
    if pvc:
        want = "ReadWriteMany" if rwx else "ReadWriteOnce"
        check(pvc[0]["spec"]["accessModes"] == [want], f"{label}: PVC access {pvc[0]['spec']['accessModes']}")
    pod = api_pod(docs)
    check(any(v.get("persistentVolumeClaim", {}).get("claimName") == "amp-receipts" for v in pod["volumes"]), f"{label}: PVC not mounted")
    check("fix-permissions" in [c["name"] for c in pod["initContainers"]], f"{label}: no fix-permissions init")
    check(seaweedfs_objects(docs) == [], f"{label}: renders SeaweedFS: {seaweedfs_objects(docs)}")


def test_seaweedfs():
    label = "seaweedfs"
    _, docs = render("--set", "seaweedfs.enabled=true", "--set", "ampApi.replicas=3")
    check(seaweedfs_objects(docs) == [("NetworkPolicy", "amp-seaweedfs"), ("Secret", "amp-seaweedfs-s3"),
                                     ("Service", "amp-seaweedfs"), ("StatefulSet", "amp-seaweedfs")],
          f"{label}: objects {seaweedfs_objects(docs)}")
    check(not find(docs, "PersistentVolumeClaim", "amp-receipts"), f"{label}: still renders the amp-receipts PVC")
    e = env(docs)
    check(e.get("RECEIPT_STORE", {}).get("value") == "s3", f"{label}: RECEIPT_STORE {e.get('RECEIPT_STORE')}")
    check(e.get("RECEIPT_S3_ENDPOINT", {}).get("value") == "amp-seaweedfs.amp.svc.cluster.local:8333", f"{label}: endpoint {e.get('RECEIPT_S3_ENDPOINT')}")
    check(e.get("RECEIPT_S3_USE_SSL", {}).get("value") == "false", f"{label}: in-cluster SeaweedFS is plain http")
    check(e.get("RECEIPT_S3_BUCKET", {}).get("value") == "amp-receipts", f"{label}: bucket {e.get('RECEIPT_S3_BUCKET')}")
    for k, key in (("RECEIPT_S3_ACCESS_KEY", "access-key"), ("RECEIPT_S3_SECRET_KEY", "secret-key")):
        ref = e.get(k, {}).get("valueFrom", {}).get("secretKeyRef", {})
        check(ref == {"name": "amp-seaweedfs-s3", "key": key} and "value" not in e.get(k, {}), f"{label}: {k} is {e.get(k)}")
    check("RECEIPT_DIR" not in e, f"{label}: RECEIPT_DIR still set")
    pod = api_pod(docs)
    check(not any("persistentVolumeClaim" in v for v in pod["volumes"]), f"{label}: amp-api mounts a PVC: {pod['volumes']}")
    check(any(v["name"] == "receipts-spool" and "emptyDir" in v for v in pod["volumes"]), f"{label}: no spool emptyDir")
    inits = [c["name"] for c in pod["initContainers"]]
    check("fix-permissions" not in inits and "wait-for-seaweedfs" in inits, f"{label}: amp-api inits {inits}")
    check(find(docs, "Deployment", "amp-api")[0]["spec"]["replicas"] == 3, f"{label}: replicas")

    sts = find(docs, "StatefulSet", "amp-seaweedfs")[0]
    spec = sts["spec"]["template"]["spec"]
    check(sts["spec"]["replicas"] == 1, f"{label}: SeaweedFS replicas {sts['spec']['replicas']}")
    c = spec["containers"][0]
    check(c["image"] == "chrislusf/seaweedfs:4.52", f"{label}: image {c['image']}")
    # Only the S3 gateway leaves loopback: master, volume and filer (no
    # auth) are bound to 127.0.0.1; Iceberg and Lance are off (I1).
    check(c["args"] == ["server", "-ip=127.0.0.1", "-ip.bind=127.0.0.1", "-s3", "-s3.ip.bind=0.0.0.0", "-s3.port=8333",
                        "-s3.port.iceberg=0", "-s3.port.lance=0", "-s3.config=/config/s3.json", "-master.telemetry=false"],
          f"{label}: args {c['args']}")
    check(not any(a.startswith(("-ip.bind=0", "-metricsPort", "-master.ip.bind", "-filer.ip.bind", "-volume.ip.bind")) for a in c["args"]),
          f"{label}: a non-S3 listener leaves loopback: {c['args']}")
    check(c["securityContext"].get("runAsNonRoot") is True and c["securityContext"].get("runAsUser") == 1000
          and c["securityContext"].get("allowPrivilegeEscalation") is False, f"{label}: securityContext {c['securityContext']}")
    check(spec["securityContext"].get("runAsNonRoot") is True and spec["securityContext"].get("fsGroup") == 1000,
          f"{label}: pod securityContext {spec['securityContext']}")
    check([p["containerPort"] for p in c["ports"]] == [8333], f"{label}: container ports {c['ports']}")
    vct = sts["spec"]["volumeClaimTemplates"][0]["spec"]
    check(vct["resources"]["requests"]["storage"] == "20Gi", f"{label}: PVC size {vct}")
    # The S3 identities file is written from the Secret, never in the spec.
    cfg = [i for i in spec["initContainers"] if i["name"] == "s3-config"]
    check(cfg and {x["name"] for x in cfg[0]["env"] if "valueFrom" in x} == {"ACCESS_KEY", "SECRET_KEY"}, f"{label}: s3-config init {cfg}")
    check(any(v["name"] == "config" and v.get("emptyDir", {}).get("medium") == "Memory" for v in spec["volumes"]), f"{label}: config volume")
    check(cfg and '[ -z "$ACCESS_KEY" ]' in cfg[0]["command"][2], f"{label}: s3-config doesn't refuse an empty key")
    sum1 = sts["spec"]["template"]["metadata"].get("annotations", {}).get("checksum/s3-keys")
    check(sum1 and len(sum1) == 64, f"{label}: no checksum/s3-keys annotation: {sts['spec']['template']['metadata']}")

    svc = find(docs, "Service", "amp-seaweedfs")[0]["spec"]
    check(svc.get("type") == "ClusterIP" and [p["port"] for p in svc["ports"]] == [8333], f"{label}: service {svc}")
    np = find(docs, "NetworkPolicy", "amp-seaweedfs")[0]["spec"]
    check(np["ingress"] == [{"from": [{"podSelector": {"matchLabels": {"app": "amp-api"}}}], "ports": [{"protocol": "TCP", "port": 8333}]}],
          f"{label}: network policy {np['ingress']}")
    sec = find(docs, "Secret", "amp-seaweedfs-s3")[0]["stringData"]
    check(len(sec.get("access-key", "")) >= 16 and len(sec.get("secret-key", "")) >= 32, f"{label}: generated keys too short")
    check(len(sec.get("jwt-signing-key", "")) >= 64, f"{label}: no SeaweedFS JWT signing key: {sorted(sec)}")
    # SeaweedFS's admin gRPC (the S3 gRPC port, which no flag keeps off the
    # pod IP) needs a token signed with this key.
    jwt = {e["name"]: e for e in c.get("env", [])}.get("WEED_JWT_FILER_SIGNING_KEY", {})
    check(jwt.get("valueFrom", {}).get("secretKeyRef") == {"name": "amp-seaweedfs-s3", "key": "jwt-signing-key", "optional": True}
          and "value" not in jwt, f"{label}: WEED_JWT_FILER_SIGNING_KEY is {jwt}")
    # Never exposed: no Envoy cluster or route to SeaweedFS, no Ingress.
    check("seaweedfs" not in envoy_config(docs), f"{label}: Envoy knows SeaweedFS")
    check(not [d for d in docs if d["kind"] in ("Ingress", "HTTPRoute")], f"{label}: ingress objects")

    # Pinned keys, an existing Secret, a bucket of its own.
    _, docs = render("--set", "seaweedfs.enabled=true", "--set", "global.secretOverrides.seaweedfsAccessKey=AKPINNED",
                     "--set", "global.secretOverrides.seaweedfsSecretKey=SKPINNED", "--set", "global.secretOverrides.seaweedfsJwtSigningKey=JWTPINNED")
    sec = find(docs, "Secret", "amp-seaweedfs-s3")[0]["stringData"]
    check(sec.get("access-key") == "AKPINNED" and sec.get("secret-key") == "SKPINNED",
          f"{label}: secretOverrides ignored: {sec}")
    check(sec.get("jwt-signing-key") == "JWTPINNED", f"{label}: JWT override ignored: {sec}")
    pinned = find(docs, "StatefulSet", "amp-seaweedfs")[0]["spec"]["template"]["metadata"]["annotations"]["checksum/s3-keys"]
    _, docs2 = render("--set", "seaweedfs.enabled=true", "--set", "global.secretOverrides.seaweedfsAccessKey=AKPINNED",
                      "--set", "global.secretOverrides.seaweedfsSecretKey=SKROTATED", "--set", "global.secretOverrides.seaweedfsJwtSigningKey=JWTPINNED")
    rotated = find(docs2, "StatefulSet", "amp-seaweedfs")[0]["spec"]["template"]["metadata"]["annotations"]["checksum/s3-keys"]
    check(pinned != rotated, f"{label}: a new key pair doesn't restart SeaweedFS")
    _, docs = render("--set", "seaweedfs.enabled=true", "--set", "seaweedfs.existingSecret=my-sw", "--set", "seaweedfs.bucket=evidence",
                     "--set", "networkPolicy.enabled=false")
    # networkPolicy.enabled=false turns the chart's other policies off, never
    # SeaweedFS's; the chart doesn't make a Secret for an existing one.
    check(seaweedfs_objects(docs) == [("NetworkPolicy", "amp-seaweedfs"), ("Service", "amp-seaweedfs"), ("StatefulSet", "amp-seaweedfs")],
          f"{label}/existing: {seaweedfs_objects(docs)}")
    check(not find(docs, "NetworkPolicy", "amp-api"), f"{label}/existing: networkPolicy.enabled=false still renders amp-api's policy")
    e = env(docs)
    check(e["RECEIPT_S3_ACCESS_KEY"]["valueFrom"]["secretKeyRef"]["name"] == "my-sw" and e["RECEIPT_S3_BUCKET"]["value"] == "evidence",
          f"{label}/existing: env {e['RECEIPT_S3_ACCESS_KEY']} {e['RECEIPT_S3_BUCKET']}")
    check("annotations" not in find(docs, "StatefulSet", "amp-seaweedfs")[0]["spec"]["template"]["metadata"],
          f"{label}/existing: a checksum of keys the chart doesn't know")
    sts = find(docs, "StatefulSet", "amp-seaweedfs")[0]["spec"]["template"]["spec"]
    cfg = [i for i in sts["initContainers"] if i["name"] == "s3-config"][0]
    check({x["name"]: x.get("valueFrom", {}).get("secretKeyRef", {}).get("name", x.get("value")) for x in cfg["env"]}
          == {"ACCESS_KEY": "my-sw", "SECRET_KEY": "my-sw", "BUCKET": "evidence"}, f"{label}/existing: s3-config env {cfg['env']}")


def test_external():
    label = "external s3"
    _, docs = render("--set", "ampApi.receipts.s3.endpoint=https://acct.r2.cloudflarestorage.com", "--set", "ampApi.receipts.s3.existingSecret=amp-r2",
                     "--set", "ampApi.receipts.s3.region=auto", "--set", "ampApi.replicas=2")
    check(seaweedfs_objects(docs) == [], f"{label}: renders SeaweedFS {seaweedfs_objects(docs)}")
    check(not find(docs, "PersistentVolumeClaim", "amp-receipts"), f"{label}: renders the amp-receipts PVC")
    e = env(docs)
    vals = {k: v.get("value") for k, v in e.items() if k.startswith("RECEIPT_")}
    check(vals.get("RECEIPT_STORE") == "s3" and vals.get("RECEIPT_S3_ENDPOINT") == "https://acct.r2.cloudflarestorage.com"
          and vals.get("RECEIPT_S3_REGION") == "auto" and vals.get("RECEIPT_S3_BUCKET") == "amp-receipts"
          and vals.get("RECEIPT_S3_USE_SSL") == "true" and vals.get("RECEIPT_S3_PATH_STYLE") == "true", f"{label}: env {vals}")
    for k, key in (("RECEIPT_S3_ACCESS_KEY", "access-key"), ("RECEIPT_S3_SECRET_KEY", "secret-key")):
        check(e[k].get("valueFrom", {}).get("secretKeyRef") == {"name": "amp-r2", "key": key}, f"{label}: {k} {e[k]}")
    pod = api_pod(docs)
    check("wait-for-seaweedfs" not in [c["name"] for c in pod["initContainers"]], f"{label}: waits for SeaweedFS")
    _, docs = render("--set", "ampApi.receipts.s3.endpoint=garage:3900", "--set", "ampApi.receipts.s3.existingSecret=g",
                     "--set", "ampApi.receipts.s3.useSSL=false", "--set", "ampApi.receipts.s3.accessKeyKey=id",
                     "--set", "ampApi.receipts.s3.secretKeyKey=secret")
    e = env(docs)
    check(e["RECEIPT_S3_USE_SSL"]["value"] == "false", f"{label}: useSSL=false {e['RECEIPT_S3_USE_SSL']}")
    check(e["RECEIPT_S3_ACCESS_KEY"]["valueFrom"]["secretKeyRef"]["key"] == "id"
          and e["RECEIPT_S3_SECRET_KEY"]["valueFrom"]["secretKeyRef"]["key"] == "secret", f"{label}: key names")


def test_refusals():
    err = render_error("--set", "ampApi.replicas=2")
    check(err and "ReadWriteMany" in err, f"local, 2 replicas, RWO: {err}")
    err = render_error("--set", "ampApi.receipts.s3.endpoint=x:1")
    check(err and "existingSecret" in err, f"external s3 without a Secret: {err}")
    err = render_error("--set", "ampApi.receipts.s3.endpoint=x:1", "--set", "ampApi.receipts.s3.existingSecret=s", "--set", "seaweedfs.enabled=true")
    check(err and "one store" in err, f"both stores: {err}")


def main():
    test_local("default")
    test_local("production", "-f", PROD, rwx=True)
    test_seaweedfs()
    test_external()
    test_refusals()
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("receipt store chart tests: all passed")


if __name__ == "__main__":
    main()
