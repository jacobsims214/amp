//! The M1-4 commands against an in-process mock AMP: doctor, whoami, tickets list/get, mcp call.
//! The `--json` outputs are pinned in full here: they are a contract for scripts (M1-5 smoke).

mod common;

use common::{MockAmp, run, spawn, workdir};
use serde_json::{Value, json};

/// doctor's model-side checks need a key for the manager's provider (the shipped roster is all
/// OpenRouter); `--offline` keeps them off the network. A fake value: it must never be printed.
const OR_KEY: (&str, &str) = ("OPENROUTER_API_KEY", "sk-or-FAKE-not-printed");

fn row(id: i64, name: &str, status: &str, label: &str, state: &str, extra: Value) -> Value {
    let mut r = json!({
        "id": id, "name": name, "status": status, "status_label": label, "state": state,
        "priority": "2", "performer": "agent", "assigned_to": "amp:amp-worker-backend",
        "owner_user_id": 7, "owner": null, "epic_id": 161, "story_id": 391,
        "blocked_by_ids": [], "question": null, "start_at": null
    });
    if let (Value::Object(r), Value::Object(extra)) = (&mut r, extra) {
        r.extend(extra);
    }
    r
}

#[tokio::test]
async fn whoami_json_and_human() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("whoami", None);

    let out = run(&dir, &amp.url, &["whoami", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(
        out.json(),
        json!({
            "user_id": 7, "email": "local-dev@example.com", "account": "human",
            "global_admin": false, "via_api_token": false,
            "orgs": [{"id": 3, "name": "simstech", "slug": "simstech", "role": "owner",
                      "teams": [{"id": 4, "name": "Engineering", "role": "lead"}]}]
        })
    );

    let out = run(&dir, &amp.url, &["whoami"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(out.stdout.starts_with("local-dev@example.com (user 7)\n"));
    assert!(out.stdout.contains(&format!("server:  {}", amp.url)));
    assert!(out.stdout.contains("auth:    none"));
    assert!(out.stdout.contains("org:     simstech (#3, owner)"));
    assert!(out.stdout.contains("team Engineering (#4, lead)"));
}

#[tokio::test]
async fn tickets_list_json_is_pinned() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("list-json", None);

    let out = run(
        &dir,
        &amp.url,
        &["tickets", "list", "--project", "2", "--json"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(
        out.json(),
        json!({
            "project_id": 2,
            "owner_filter": "mine",
            "states": ["question", "in_progress", "ready", "scheduled", "waiting"],
            "total": 7,
            "count": 6,
            "tickets": [
                row(2084, "M1-3 OAuth", "question", "Question", "blocked",
                    json!({"question": "Keychain or file?"})),
                row(2085, "M1-4 CLI", "in_progress", "In progress", "in_progress", json!({})),
                row(2086, "M1-5 Smoke test", "ready", "Ready", "backlog", json!({})),
                {
                    "id": 2092, "name": "Merge PR M1", "status": "ready", "status_label": "Ready",
                    "state": "backlog", "priority": null, "performer": "human", "assigned_to": null,
                    "owner_user_id": null, "owner": "jacob@example.com", "epic_id": null,
                    "story_id": null, "blocked_by_ids": [], "question": null, "start_at": null
                },
                row(2090, "Later", "scheduled", "Scheduled", "blocked",
                    json!({"assigned_to": null, "start_at": "2026-11-01T09:00:00Z"})),
                row(2087, "M1-6 KB how-to", "waiting", "Waiting", "blocked",
                    json!({"assigned_to": "amp:amp-worker-docs", "blocked_by_ids": [2086]})),
            ]
        })
    );
    // The CLI asks for every bucket and leaves AMP's owner default alone.
    assert_eq!(
        amp.mock.calls_to("amp_list_tasks"),
        [json!({"project_id": 2, "format": "full"})]
    );
}

#[tokio::test]
async fn tickets_list_filters_owner_and_project_from_marker() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("list-filter", Some(2));

    // No --project: the .amp.json in the working directory names it (its amp_api is ignored).
    let out = run(
        &dir,
        &amp.url,
        &[
            "tickets",
            "list",
            "--state",
            "done,question",
            "--owner",
            "any",
            "--json",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let v = out.json();
    assert_eq!(v["owner_filter"], "any");
    assert_eq!(v["states"], json!(["question", "done"]));
    let ids: Vec<i64> = v["tickets"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["id"].as_i64().unwrap())
        .collect();
    assert_eq!(ids, [2084, 2083]);
    assert_eq!(
        amp.mock.calls_to("amp_list_tasks"),
        [json!({"project_id": 2, "format": "full", "owner": "any"})]
    );

    // Human output: grouped under the web UI's labels.
    let out = run(&dir, &amp.url, &["tickets", "list"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let s = &out.stdout;
    assert!(
        s.starts_with(
            "Project 2 · owner: mine · 6 tickets (1 more hidden; --state all shows them)\n"
        ),
        "{s}"
    );
    for want in [
        "\nQuestion (1)\n",
        "\nIn progress (1)\n",
        "\nReady (2)\n",
        "\nScheduled (1)\n",
        "\nWaiting (1)\n",
        "#2084  M1-3 OAuth  [amp:amp-worker-backend]",
        "? Keychain or file?",
        "#2092  Merge PR M1  [person: jacob@example.com]",
        "waits on #2086",
        "starts 2026-11-01T09:00:00Z",
    ] {
        assert!(s.contains(want), "missing {want:?} in:\n{s}");
    }
    let order: Vec<usize> = ["Question", "In progress", "Ready", "Scheduled", "Waiting"]
        .iter()
        .map(|l| s.find(&format!("\n{l} (")).unwrap())
        .collect();
    assert!(order.windows(2).all(|w| w[0] < w[1]), "{s}");
    assert!(!s.contains("Done ("), "{s}");
}

#[tokio::test]
async fn tickets_list_needs_a_project() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("list-noproject", None);
    let out = run(&dir, &amp.url, &["tickets", "list"], &[]).await;
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("pass --project N"), "{}", out.stderr);

    let out = run(
        &dir,
        &amp.url,
        &["tickets", "list", "--state", "bogus"],
        &[],
    )
    .await;
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("unknown state"), "{}", out.stderr);
}

#[tokio::test]
async fn tickets_get_json_is_pinned() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("get-json", None);

    let detail = |extra: Value| {
        let mut t = row(
            2085,
            "M1-4 CLI",
            "in_progress",
            "In progress",
            "in_progress",
            json!({}),
        );
        t.as_object_mut().unwrap().extend(
            json!({
                "project_id": 2,
                "description": "Build the CLI.\nSecond line.",
                "acceptance_criteria": "- doctor works",
                "dependency_ids": [2084],
                "dispatched_at": "2026-10-22T14:17:16Z",
                "completed_at": null,
                "created_at": "2026-10-22T13:17:39Z",
                "updated_at": "2026-10-22T14:17:16Z"
            })
            .as_object()
            .unwrap()
            .clone(),
        );
        let mut out = json!({"ticket": t});
        out.as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        out
    };

    let out = run(&dir, &amp.url, &["tickets", "get", "2085", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.json(), detail(json!({})));

    let out = run(
        &dir,
        &amp.url,
        &["tickets", "get", "#2085", "--comments", "--json"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(
        out.json(),
        detail(json!({
            "comments": [{"id": 1, "author": "amp-worker", "body": "Starting work.",
                          "created_at": "2026-10-22T14:20:00Z"}],
            "comments_total": 1
        }))
    );
    assert_eq!(
        amp.mock.calls_to("amp_get_task"),
        [
            json!({"task_id": 2085}),
            json!({"task_id": 2085, "include": ["comments"]})
        ]
    );
}

#[tokio::test]
async fn tickets_get_human_and_errors() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("get-human", None);

    let out = run(
        &dir,
        &amp.url,
        &["tickets", "get", "2085", "--comments"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    for want in [
        "#2085 M1-4 CLI\n",
        "status:     In progress (in_progress)",
        "in:         project 2 · epic 161 · story 391",
        "worked by:  amp:amp-worker-backend",
        "depends on: #2084",
        "\nDescription\n  Build the CLI.\n  Second line.\n",
        "\nAcceptance criteria\n  - doctor works\n",
        "\nComments (1 of 1)\n  --- amp-worker 2026-10-22T14:20:00Z\n  Starting work.\n",
    ] {
        assert!(
            out.stdout.contains(want),
            "missing {want:?} in:\n{}",
            out.stdout
        );
    }

    let out = run(&dir, &amp.url, &["tickets", "get", "999"], &[]).await;
    assert_eq!(out.code, 1);
    assert_eq!(out.stdout, "");
    assert!(
        out.stderr.contains("amp_get_task: task not found"),
        "{}",
        out.stderr
    );

    let out = run(&dir, &amp.url, &["tickets", "get", "abc"], &[]).await;
    assert_eq!(out.code, 2);
}

#[tokio::test]
async fn mcp_call_prints_json_text_raw_and_errors() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("mcp-call", None);

    let out = run(&dir, &amp.url, &["mcp", "call", "amp_whoami"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.json(), common::whoami_json());

    let out = run(
        &dir,
        &amp.url,
        &["mcp", "call", "amp_get_project", r#"{"project_id": 2}"#],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.json()["project"]["name"], "AMP v2");

    let out = run(&dir, &amp.url, &["mcp", "call", "amp_kb_search", "{}"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.stdout, "plain text, not JSON\n");

    let out = run(&dir, &amp.url, &["mcp", "call", "--raw", "amp_whoami"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let raw = out.json();
    assert_eq!(raw["content"][0]["type"], "text");
    assert_eq!(
        serde_json::from_str::<Value>(raw["content"][0]["text"].as_str().unwrap()).unwrap(),
        common::whoami_json()
    );

    // A tool error: message on stderr, exit 1.
    let out = run(&dir, &amp.url, &["mcp", "call", "amp_fail"], &[]).await;
    assert_eq!(out.code, 1);
    assert_eq!(out.stdout, "");
    assert_eq!(out.stderr, "amp_fail: it broke\n");

    // A JSON-RPC error (unknown tool): exit 1 with the server's message.
    let out = run(&dir, &amp.url, &["mcp", "call", "amp_nope"], &[]).await;
    assert_eq!(out.code, 1);
    assert!(
        out.stderr.contains("unknown tool amp_nope"),
        "{}",
        out.stderr
    );

    // Bad arguments never reach the server.
    let out = run(&dir, &amp.url, &["mcp", "call", "amp_whoami", "[1]"], &[]).await;
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("JSON object"), "{}", out.stderr);

    // Hidden from --help.
    let out = run(&dir, &amp.url, &["--help"], &[]).await;
    assert!(!out.stdout.contains("\n  mcp "), "{}", out.stdout);
    assert!(out.stdout.contains("doctor") && out.stdout.contains("tickets"));
}

#[tokio::test]
async fn mcp_tools_lists_the_catalog() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("mcp-tools", None);
    let out = run(&dir, &amp.url, &["mcp", "tools", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let names: Vec<String> = out
        .json()
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap().to_owned())
        .collect();
    assert_eq!(names.len(), common::CONTRACT.len() + 2);
    assert!(names.windows(2).all(|w| w[0] < w[1]), "sorted: {names:?}");
}

#[tokio::test]
async fn doctor_json_is_pinned() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("doctor-json", Some(2));
    let creds = dir.join("home/.config/amp-harness/credentials.json");
    let marker = dir.join("project/.amp.json");

    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(!out.stdout.contains(OR_KEY.1));
    let mut v = out.json();
    // Build-dependent parts are checked here, the rest is pinned in full below.
    let versions = v.as_object_mut().unwrap().remove("versions").unwrap();
    assert_eq!(versions["harness"], env!("CARGO_PKG_VERSION"));
    assert_eq!(versions["mcp_protocol"], "2026-07-28");
    assert!(versions["rmcp"].as_str().unwrap().starts_with("3."));
    assert!(
        versions["assets_commit"]
            .as_str()
            .is_some_and(|c| !c.is_empty())
    );
    assert_eq!(versions["assets_source_sha256"].as_str().unwrap().len(), 64);
    let models = v.as_object_mut().unwrap().remove("models").unwrap();
    assert_eq!(models["mode"], "profile");
    assert_eq!(models["selection"], "opencode");
    assert_eq!(models["config_found"], false);
    assert_eq!(models["providers"][0]["id"], "openrouter");
    assert_eq!(models["providers"][0]["key"], "env");
    assert_eq!(
        models["providers"][0]["key_source"],
        "env OPENROUTER_API_KEY"
    );
    assert_eq!(models["providers"][0]["catalog_checked"], false);
    let checks = v["checks"].as_array_mut().unwrap();
    let model_checks: Vec<Value> = checks.drain(9..).collect();
    let tail: Vec<(&str, &str)> = model_checks
        .iter()
        .map(|c| (c["name"].as_str().unwrap(), c["status"].as_str().unwrap()))
        .collect();
    assert_eq!(
        tail,
        [
            ("roles", "ok"),
            ("key:openrouter", "ok"),
            ("models:openrouter", "skip")
        ]
    );
    assert_eq!(model_checks[1]["detail"], "key from env OPENROUTER_API_KEY");
    let versions_check = checks[4]["detail"].as_str().unwrap().to_owned();
    assert!(
        versions_check.starts_with(&format!("harness {}, rpi ", env!("CARGO_PKG_VERSION")))
            && versions_check.ends_with(", MCP 2026-07-28"),
        "{versions_check}"
    );
    checks[4]["detail"] = json!("<versions>");
    assert_eq!(
        v,
        json!({
            "ok": true,
            "harness_version": env!("CARGO_PKG_VERSION"),
            "server": {
                "url": amp.url, "url_source": "env AMP_MCP_URL",
                "ca_file": null, "ca_file_source": null,
                "protocol_mode": "auto", "protocol_source": "default",
                "config_file": null
            },
            "auth": {
                "method": "none",
                "credential_store": format!("file {}", creds.display()),
                "stored_login": false
            },
            "connection": {
                "reachable": true, "connected": true,
                "protocol_version": "2026-07-28", "era": "stateless", "fell_back": false,
                "server_name": "amp-mock", "server_version": "1.2.3",
                "instructions": true, "error": null
            },
            "tools": {"count": common::CONTRACT.len() + 2, "missing_contract": []},
            "whoami": {"user_id": 7, "email": "local-dev@example.com", "account": "human"},
            "project": {
                "marker": marker.display().to_string(), "id": 2, "name": "AMP v2",
                "marker_name": "AMP v2", "found": true
            },
            "ca": {
                "trust": "none", "file": null, "file_source": null, "certificates": [],
                "tls_error": null
            },
            "checks": [
                {"name": "config", "status": "ok", "detail": format!("server {}", amp.url), "fix": null},
                {"name": "credentials", "status": "warn", "detail": format!(
                    "no OS keychain; logins are kept in file {} (mode 0600)", creds.display()),
                 "fix": "unset AMP_CREDENTIAL_STORE to keep logins in the OS keychain instead"},
                {"name": "ca", "status": "skip", "detail": "plain http: no TLS", "fix": null},
                {"name": "connect", "status": "ok",
                 "detail": "MCP 2026-07-28 (stateless), server amp-mock 1.2.3", "fix": null},
                {"name": "versions", "status": "ok", "detail": "<versions>", "fix": null},
                {"name": "tools", "status": "ok", "detail": format!("{} tools", common::CONTRACT.len() + 2), "fix": null},
                {"name": "contract", "status": "ok", "detail": "all 15 contract tools present", "fix": null},
                {"name": "whoami", "status": "ok", "detail": "local-dev@example.com (user 7)", "fix": null},
                {"name": "project", "status": "ok",
                 "detail": format!("#2 AMP v2 (from {})", marker.display()), "fix": null}
            ]
        })
    );
}

#[tokio::test]
async fn doctor_reports_legacy_fallback_and_forced_protocol() {
    let amp = spawn(
        MockAmp {
            legacy_only: true,
            ..MockAmp::new()
        },
        None,
    )
    .await;
    let dir = workdir("doctor-legacy", None);
    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stdout);
    let v = out.json();
    assert_eq!(v["connection"]["protocol_version"], "2025-11-25");
    assert_eq!(v["connection"]["era"], "legacy");
    assert_eq!(v["connection"]["fell_back"], true);
    assert_eq!(v["project"], Value::Null);
    let project = v["checks"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["name"] == "project")
        .unwrap()
        .clone();
    assert_eq!(project["status"], "skip");

    let modern = spawn(MockAmp::new(), None).await;
    let out = run(
        &dir,
        &modern.url,
        &["doctor", "--json", "--offline", "--protocol", "2025-11-25"],
        &[OR_KEY],
    )
    .await;
    let v = out.json();
    assert_eq!(v["server"]["protocol_mode"], "2025-11-25");
    assert_eq!(v["server"]["protocol_source"], "flag");
    assert_eq!(v["connection"]["era"], "legacy");
    assert_eq!(v["connection"]["fell_back"], false);
}

#[tokio::test]
async fn doctor_reports_missing_contract_tools() {
    let amp = spawn(
        MockAmp {
            omit: vec!["amp_report_run", "amp_kb_write"],
            ..MockAmp::new()
        },
        None,
    )
    .await;
    let dir = workdir("doctor-contract", None);
    let out = run(&dir, &amp.url, &["doctor", "--json"], &[]).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(v["ok"], false);
    assert_eq!(
        v["tools"]["missing_contract"],
        json!(["amp_report_run", "amp_kb_write"])
    );

    let out = run(&dir, &amp.url, &["doctor", "--offline"], &[OR_KEY]).await;
    assert_eq!(out.code, 1);
    let line = out
        .stdout
        .lines()
        .find(|l| l.contains("[FAIL] contract"))
        .unwrap_or_else(|| panic!("{}", out.stdout));
    assert!(
        line.ends_with(" missing: amp_report_run, amp_kb_write"),
        "{line}"
    );
    assert!(
        out.stdout
            .contains("fix: this AMP server (1.2.3) lacks tools"),
        "{}",
        out.stdout
    );
    assert!(out.stdout.contains("Failed: contract\n"), "{}", out.stdout);
}

#[tokio::test]
async fn doctor_with_a_static_token_never_prints_it() {
    const TOKEN: &str = "amp_secret_value_123";
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let dir = workdir("doctor-token", None);

    // Without the token: reachable, but a login is needed.
    let out = run(&dir, &amp.url, &["doctor", "--json"], &[]).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(v["connection"]["reachable"], true);
    assert_eq!(v["connection"]["connected"], false);
    let connect = &v["checks"][3];
    assert_eq!(
        (&connect["name"], &connect["status"]),
        (&json!("connect"), &json!("fail"))
    );
    assert_eq!(
        connect["fix"],
        format!(
            "run `amp-harness login --url {}` (or export AMP_TOKEN=<token from the AMP web UI>)",
            amp.url
        )
    );
    let out = run(&dir, &amp.url, &["doctor"], &[]).await;
    assert!(
        out.stdout.contains("Hint: run `amp-harness login`"),
        "{}",
        out.stdout
    );
    let out = run(&dir, &amp.url, &["whoami"], &[]).await;
    assert_eq!(out.code, 1);
    assert!(out.stderr.contains("AMP login required"), "{}", out.stderr);

    // With it: connected, described by its source only.
    for args in [
        &["doctor", "--json", "--offline"][..],
        &["doctor", "--offline"][..],
        &["whoami"][..],
    ] {
        let out = run(&dir, &amp.url, args, &[("AMP_TOKEN", TOKEN), OR_KEY]).await;
        assert_eq!(out.code, 0, "{args:?}: {}{}", out.stdout, out.stderr);
        assert!(
            out.stdout.contains("static token (env AMP_TOKEN)"),
            "{args:?}: {}",
            out.stdout
        );
        assert!(!out.stdout.contains(TOKEN) && !out.stderr.contains(TOKEN));
    }
    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[("AMP_TOKEN", TOKEN), OR_KEY],
    )
    .await;
    let v = out.json();
    assert_eq!(v["auth"]["credential_store"], Value::Null);
    assert_eq!(
        v["checks"][1]["detail"],
        "static token from AMP_TOKEN (not stored)"
    );
}

#[tokio::test]
async fn doctor_fails_cleanly_when_nothing_answers() {
    let dir = workdir("doctor-down", None);
    // A port nothing listens on.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/mcp", listener.local_addr().unwrap());
    drop(listener);
    let out = run(&dir, &url, &["doctor", "--json"], &[]).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(v["connection"]["reachable"], false);
    assert!(v["connection"]["error"].is_string());
    let statuses: Vec<(&str, &str)> = v["checks"]
        .as_array()
        .unwrap()
        .iter()
        .map(|c| (c["name"].as_str().unwrap(), c["status"].as_str().unwrap()))
        .collect();
    assert_eq!(
        statuses,
        [
            ("config", "ok"),
            ("credentials", "warn"),
            ("ca", "skip"),
            ("connect", "fail"),
            ("versions", "ok"),
            ("tools", "skip"),
            ("contract", "skip"),
            ("whoami", "skip"),
            ("project", "skip"),
            // The model side still runs: no OpenRouter key for the manager.
            ("roles", "ok"),
            ("key:openrouter", "fail"),
            ("models:openrouter", "skip")
        ]
    );
    let fix = v["checks"][3]["fix"].as_str().unwrap();
    assert!(
        fix.contains("--url") && fix.contains("AMP_MCP_URL"),
        "{fix}"
    );
    assert!(
        v["checks"][10]["fix"]
            .as_str()
            .unwrap()
            .starts_with("set OPENROUTER_API_KEY in the environment"),
        "{}",
        v["checks"][10]
    );

    // A bad URL fails at config.
    let out = run(&dir, "not a url", &["doctor", "--json"], &[]).await;
    assert_eq!(out.code, 1);
    assert_eq!(out.json()["checks"][0]["status"], "fail");
    assert_eq!(out.json()["server"], Value::Null);
}
