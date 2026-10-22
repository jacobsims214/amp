//! #2178: a failure to reach AMP reads as one plain sentence with a next step, never rmcp's
//! `Send message error Transport [rmcp::…<reqwest::…>]`; `--verbose` (or RUST_LOG) adds the cause
//! chain. The chat opens offline instead of exiting, and connects once AMP answers.
//! Mock AMPs in-process, HOME under cargo's target dir (D13).

mod common;

use std::{
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::json;

/// A token-shaped value that must never be printed.
const TOKEN: &str = "amp_test_2178_never_print_me";

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

/// A loopback port nothing listens on.
async fn closed_port() -> u16 {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    listener.local_addr().unwrap().port()
}

/// The person-facing text has no Rust type names and no rmcp/reqwest wording.
fn assert_plain(text: &str) {
    for raw in [
        "::",
        "rmcp",
        "reqwest",
        "Transport [",
        "Send message error",
        "os error",
    ] {
        assert!(!text.contains(raw), "{raw:?} in\n{text}");
    }
}

/// One line on stderr: `amp-harness: <the sentence>`, exit 1.
fn assert_one_sentence(out: &Output, want: &str) {
    assert_eq!(
        out.code, 1,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let err = out.stderr.trim_end();
    assert_eq!(err.lines().count(), 1, "one line, got\n{err}");
    assert!(err.starts_with("amp-harness: "), "{err}");
    assert!(err.contains(want), "missing {want:?} in\n{err}");
    assert_plain(err);
}

fn write_faux_config(dir: &Path) {
    let path = dir.join("home/.config/amp-harness/config.toml");
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, FAUX_CONFIG).unwrap();
}

#[tokio::test]
async fn a_closed_port_is_one_sentence_with_a_next_step() {
    let port = closed_port().await;
    let url = format!("http://127.0.0.1:{port}/mcp");
    let dir = workdir("connect-refused", None);
    let out = run(&dir, &url, &["whoami"], &[]).await;
    assert_one_sentence(
        &out,
        &format!(
            "Can't reach AMP at 127.0.0.1:{port} (connection refused). Is the server running?"
        ),
    );
    assert!(
        out.stderr.contains("`amp-harness doctor`"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr.contains("`amp-harness server set <url>`"),
        "{}",
        out.stderr
    );

    // The same for the other one-shot commands and for a headless run.
    for args in [
        &["tickets", "list", "--project", "2"][..],
        &["run", "-p", "hi"],
    ] {
        let out = run(&dir, &url, args, &[]).await;
        assert_eq!(out.code, 1, "{args:?}: {}", out.stderr);
        assert!(
            out.stderr.contains("(connection refused)"),
            "{args:?}: {}",
            out.stderr
        );
        assert_plain(&out.stderr);
    }
}

#[tokio::test]
async fn verbose_and_rust_log_still_show_the_cause_chain() {
    let port = closed_port().await;
    let url = format!("http://127.0.0.1:{port}/mcp");
    let dir = workdir("connect-verbose", None);
    for (args, env) in [
        (&["whoami", "--verbose"][..], &[][..]),
        (&["whoami"][..], &[("RUST_LOG", "debug")][..]),
    ] {
        let out = run(&dir, &url, args, env).await;
        assert_eq!(out.code, 1);
        assert!(
            out.stderr.contains("(connection refused)"),
            "{}",
            out.stderr
        );
        let cause = out
            .stderr
            .lines()
            .find(|l| l.trim_start().starts_with("cause: "))
            .unwrap_or_else(|| panic!("no cause line in\n{}", out.stderr));
        assert!(cause.contains(&url), "{cause}");
        assert!(cause.contains("Connection refused"), "{cause}");
    }
}

#[tokio::test]
async fn an_unknown_host_is_host_not_found() {
    let dir = workdir("connect-dns", None);
    let out = run(&dir, "http://amp-2178.invalid/mcp", &["whoami"], &[]).await;
    assert_one_sentence(
        &out,
        "Can't reach AMP at amp-2178.invalid (host not found).",
    );
    assert!(
        out.stderr.contains("`amp-harness server`"),
        "{}",
        out.stderr
    );
}

#[tokio::test]
async fn a_401_says_to_log_in_or_names_the_refused_token() {
    let amp = spawn(MockAmp::new(), Some("amp_right_2178")).await;
    let dir = workdir("connect-401", None);
    let out = run(&dir, &amp.url, &["whoami"], &[]).await;
    assert_one_sentence(
        &out,
        "needs you to log in (401). Run `amp-harness login --url",
    );

    let out = run(&dir, &amp.url, &["whoami"], &[("AMP_TOKEN", TOKEN)]).await;
    assert_one_sentence(
        &out,
        "refused the token in AMP_TOKEN (401). Set a new token in AMP_TOKEN",
    );
    assert!(!out.stderr.contains(TOKEN) && !out.stdout.contains(TOKEN));
}

#[tokio::test]
async fn a_wrong_path_is_a_404_with_the_address_to_check() {
    let amp = spawn(MockAmp::new(), None).await;
    let url = amp.url.replace("/mcp", "/nope");
    let dir = workdir("connect-404", None);
    let out = run(&dir, &url, &["whoami"], &[]).await;
    assert_one_sentence(
        &out,
        "(HTTP 404). Check the address (it usually ends in /mcp)",
    );
}

/// The chat does not exit: it opens offline, says why with a next step, and the model still
/// answers (without AMP tools).
#[tokio::test]
async fn the_chat_opens_offline_when_amp_is_unreachable() {
    let port = closed_port().await;
    let url = format!("http://127.0.0.1:{port}/mcp");
    let dir = workdir("chat-offline", None);
    write_faux_config(&dir);
    let script = dir.join("script.json");
    std::fs::write(&script, json!([{"text": "Hello without AMP."}]).to_string()).unwrap();
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        &url,
        &["--snapshot", "200x40", "--no-setup", "--input", "hi"],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let screen = &out.stdout;
    for want in [
        format!("{url} (not connected)"),
        format!("Can't reach AMP at 127.0.0.1:{port} (connection refused)."),
        "keeps retrying".to_owned(),
        "`/server <name|address>` switches from here".to_owned(),
        "Hello without AMP.".to_owned(),
    ] {
        assert!(screen.contains(&want), "missing {want:?} in\n{screen}");
    }
    assert_plain(screen);
}

/// The offline chat connects once AMP comes up: the transcript says so, and the model's AMP
/// tools work (its `amp_whoami` reaches the server).
#[tokio::test]
async fn the_offline_chat_connects_when_amp_comes_up() {
    // AMP is "down" (503) until the chat has tried it, then serves.
    let mock = MockAmp::new();
    let down = Arc::new(AtomicBool::new(true));
    let tries = Arc::new(AtomicUsize::new(0));
    let app = {
        let (down, tries) = (down.clone(), tries.clone());
        common::app(&mock, None).layer(axum::middleware::from_fn(
            move |req: axum::extract::Request, next: axum::middleware::Next| {
                let (down, tries) = (down.clone(), tries.clone());
                async move {
                    if down.load(Ordering::SeqCst) {
                        tries.fetch_add(1, Ordering::SeqCst);
                        return axum::response::IntoResponse::into_response((
                            axum::http::StatusCode::SERVICE_UNAVAILABLE,
                            "starting",
                        ));
                    }
                    next.run(req).await
                }
            },
        ))
    };
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let server = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let url = format!("http://{addr}/mcp");
    let dir = workdir("chat-offline-retry", None);
    write_faux_config(&dir);
    let script = dir.join("script.json");
    std::fs::write(
        &script,
        json!([
            {"text": "Checking.", "tool_calls": [{"name": "amp_whoami", "id": "call-1"}]},
            {"text": "Done."}
        ])
        .to_string(),
    )
    .unwrap();
    let state = dir.join("home/.local/state");

    let chat = {
        let (dir, url, script, state) = (dir.clone(), url.clone(), script.clone(), state.clone());
        tokio::spawn(async move {
            run(
                &dir,
                &url,
                &[
                    "--snapshot",
                    "200x50",
                    "--no-setup",
                    "--input",
                    "wait:amp",
                    "--input",
                    "who am I?",
                ],
                &[
                    ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
                    ("XDG_STATE_HOME", state.to_str().unwrap()),
                ],
            )
            .await
        })
    };
    // AMP comes up once the chat failed to reach it.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
    while tries.load(Ordering::SeqCst) == 0 {
        assert!(
            tokio::time::Instant::now() < deadline,
            "the chat never tried AMP"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // Let the start's whole attempt (discover, then the legacy fallback) fail first; the first
    // retry is 2 s in.
    tokio::time::sleep(Duration::from_millis(500)).await;
    down.store(false, Ordering::SeqCst);

    let out = chat.await.unwrap();
    server.abort();
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let screen = &out.stdout;
    for want in [
        format!("The server at {addr} did not answer as AMP (it answered HTTP 503, not MCP)."),
        "keeps retrying".to_owned(),
        format!("Connected to AMP at {addr}: the manager has its"),
        "✓ whoami".to_owned(),
        "Done.".to_owned(),
    ] {
        assert!(screen.contains(&want), "missing {want:?} in\n{screen}");
    }
    assert_eq!(mock.calls_to("amp_whoami").len(), 1, "the model's call");
}

/// The chat's first run used to stop at "could not connect to …" when the configured server was
/// down; now it hands over to the chat, which opens offline.
#[tokio::test]
async fn the_first_run_hands_an_unreachable_server_to_the_offline_chat() {
    let port = closed_port().await;
    let url = format!("http://127.0.0.1:{port}/mcp");
    let dir = workdir("chat-first-run-offline", None);
    let path = dir.join("home/.config/amp-harness/config.toml");
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(&path, format!("[server]\nurl = \"{url}\"\n{FAUX_CONFIG}")).unwrap();
    let script = dir.join("script.json");
    std::fs::write(&script, json!([{"text": "Hello."}]).to_string()).unwrap();
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        "",
        &["--snapshot", "200x40", "--input", "hi"],
        &[
            ("AMP_HARNESS_PROMPTS", "stdin"),
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    assert!(
        out.stdout.contains(&format!(
            "Can't reach AMP at 127.0.0.1:{port} (connection refused)."
        )),
        "{}",
        out.stdout
    );
    assert!(out.stdout.contains("Hello."), "{}", out.stdout);
    assert_plain(&out.stdout);
    assert_plain(&out.stderr);
}
