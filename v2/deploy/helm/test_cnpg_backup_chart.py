#!/usr/bin/env python3
"""Helm template tests for CNPG backups to an S3-compatible store (#145).

Default: no backup block and no ScheduledBackup. cnpg.backup.enabled: the
Cluster gets backup.barmanObjectStore with keys from an existing Secret, plus
a ScheduledBackup. Enabling without a destination or Secret is refused.

Usage (needs helm, PyYAML, and the valkey subchart in deploy/helm/amp/charts):

    python3 v2/deploy/helm/test_cnpg_backup_chart.py
"""
import os
import sys

from test_relay_chart import check, failures, find, render, render_error

PROD = os.path.join(os.path.dirname(os.path.abspath(__file__)), "amp", "values-production.yaml")
ON = ["--set", "cnpg.backup.enabled=true", "--set", "cnpg.backup.destinationPath=s3://amp-backups/",
      "--set", "cnpg.backup.existingSecret=amp-backup-creds"]


def cluster(docs):
    return find(docs, "Cluster", "amp-postgres")[0]["spec"]


def test_off(label, *args):
    _, docs = render(*args)
    check("backup" not in cluster(docs), f"{label}: backup block rendered")
    check(not find(docs, "ScheduledBackup", "amp-postgres-daily"), f"{label}: ScheduledBackup rendered")


def test_on():
    _, docs = render(*ON, "--set", "cnpg.backup.endpointURL=https://s3.example")
    b = cluster(docs)["backup"]
    o = b["barmanObjectStore"]
    check(o["destinationPath"] == "s3://amp-backups/" and o["endpointURL"] == "https://s3.example", f"store {o}")
    check(o["s3Credentials"]["accessKeyId"] == {"name": "amp-backup-creds", "key": "ACCESS_KEY_ID"}, f"access key {o['s3Credentials']}")
    check(o["s3Credentials"]["secretAccessKey"] == {"name": "amp-backup-creds", "key": "SECRET_ACCESS_KEY"}, "secret key")
    check(b["retentionPolicy"] == "30d", f"retention {b['retentionPolicy']}")
    sb = find(docs, "ScheduledBackup", "amp-postgres-daily")
    check(len(sb) == 1, "no ScheduledBackup")
    if sb:
        s = sb[0]["spec"]
        check(s["schedule"] == "0 0 3 * * *" and s["cluster"]["name"] == "amp-postgres"
              and s["method"] == "barmanObjectStore" and s["backupOwnerReference"] == "self", f"scheduled {s}")
    _, docs = render(*ON)
    check("endpointURL" not in cluster(docs)["backup"]["barmanObjectStore"], "endpointURL set without a value")
    _, docs = render("-f", PROD, "--set", "cnpg.backup.enabled=true")
    check(cluster(docs)["backup"]["barmanObjectStore"]["endpointURL"].startswith("https://seaweedfs"), "production values")


def test_refusals():
    err = render_error("--set", "cnpg.backup.enabled=true", "--set", "cnpg.backup.existingSecret=s")
    check(err and "destinationPath" in err, f"no destination: {err}")
    err = render_error("--set", "cnpg.backup.enabled=true", "--set", "cnpg.backup.destinationPath=s3://b/")
    check(err and "existingSecret" in err, f"no secret: {err}")


REC = ["--set", "cnpg.recovery.enabled=true", "--set", "cnpg.recovery.sourcePath=s3://old/",
       "--set", "cnpg.backup.enabled=true", "--set", "cnpg.backup.destinationPath=s3://new/",
       "--set", "cnpg.backup.existingSecret=amp-backup-creds"]


def test_recovery():
    _, docs = render()
    check("recovery" not in cluster(docs)["bootstrap"] and "initdb" in cluster(docs)["bootstrap"], "default is initdb")
    check("externalClusters" not in cluster(docs), "default has externalClusters")
    _, docs = render(*REC, "--set", "cnpg.recovery.targetTime=2026-11-04 14:30:00+00")
    c = cluster(docs)
    r = c["bootstrap"]["recovery"]
    check("initdb" not in c["bootstrap"], "initdb and recovery both set")
    check(r["database"] == "amp" and r["owner"] == "amp", f"recovery db/owner {r}")
    check(r["recoveryTarget"]["targetTime"] == "2026-11-04 14:30:00+00", f"target {r}")
    e = c["externalClusters"][0]
    check(e["name"] == r["source"], "source name mismatch")
    check(e["barmanObjectStore"]["destinationPath"] == "s3://old/" and e["barmanObjectStore"]["serverName"] == "amp-postgres", f"source {e}")
    check(c["backup"]["barmanObjectStore"]["destinationPath"] == "s3://new/", "archives to new path")
    _, docs = render(*REC)
    check("recoveryTarget" not in cluster(docs)["bootstrap"]["recovery"], "recoveryTarget without a time")
    _, docs = render(*REC, "--set", "relay.enabled=true")
    check(bool(cluster(docs).get("managed", {}).get("roles")), "relay role dropped on recovery")
    err = render_error("--set", "cnpg.recovery.enabled=true", "--set", "cnpg.recovery.sourcePath=s3://old/")
    check(err and "cnpg.backup.enabled" in err, f"recovery without backup: {err}")
    err = render_error(*ON, "--set", "cnpg.recovery.enabled=true")
    check(err and "sourcePath" in err, f"no sourcePath: {err}")
    err = render_error(*ON, "--set", "cnpg.recovery.enabled=true", "--set", "cnpg.recovery.sourcePath=s3://amp-backups/")
    check(err and "must differ" in err, f"same path: {err}")


def main():
    test_recovery()
    test_off("default")
    test_off("production", "-f", PROD)
    test_on()
    test_refusals()
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("cnpg backup chart tests: all passed")


if __name__ == "__main__":
    main()
