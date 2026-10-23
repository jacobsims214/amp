//! #2250: every model request carries the current date, time and timezone, for the manager and
//! for workers, and none of it reaches the transcript, the session JSONL or the output. `run -p`
//! end to end on the faux provider: the script echoes `{{current_time}}`, the newest clock stamp the
//! provider was sent. (Worker runs under the supervisor: tests/supervisor.rs.)

mod common;

use std::path::Path;

use chrono::{DateTime, NaiveDateTime, Utc};
use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

fn setup(name: &str, script: Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn harness(dir: &Path, url: &str, args: &[&str]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    run(
        dir,
        url,
        args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
            ("TZ", "America/New_York"),
        ],
    )
    .await
}

/// The stamp the model echoed after `label=` in `text`.
fn echoed<'a>(text: &'a str, label: &str) -> &'a str {
    let start = text
        .find(&format!("{label}="))
        .unwrap_or_else(|| panic!("no {label}= in {text}"))
        + label.len()
        + 1;
    let rest = &text[start..];
    &rest[..rest.find(['|', '"', '\n']).unwrap_or(rest.len())]
}

/// A stamp is the local time with its offset and zone, and UTC within a few minutes of now.
fn assert_current(stamp: &str) {
    assert!(stamp.contains(" America/New_York ("), "{stamp}");
    assert!(
        stamp.contains(" -04:00 ") || stamp.contains(" -05:00 "),
        "{stamp}"
    );
    let utc = stamp
        .rsplit_once('(')
        .and_then(|(_, u)| u.strip_suffix(" UTC)"))
        .unwrap_or_else(|| panic!("no UTC part: {stamp}"));
    let utc: DateTime<Utc> = NaiveDateTime::parse_from_str(utc, "%Y-%m-%d %H:%M:%S")
        .unwrap()
        .and_utc();
    let skew = (Utc::now() - utc).num_seconds().abs();
    assert!(skew < 300, "{stamp} is {skew}s from now");
}

/// Runs `role` on a prompt, an AMP tool call and an answer, each turn echoing its stamp.
async fn check_role(name: &str, role: &str) {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        name,
        json!([
            {"text": "t1={{current_time}}|", "tool_calls": [{"name": "amp_whoami", "id": "call-1"}]},
            {"text": "t2={{current_time}}|r={{last_tool_result}}"}
        ]),
    );
    let out = harness(
        &dir,
        &amp.url,
        &["run", "-p", "what time is it?", "--role", role, "--json"],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );

    // What the model answered, per turn (the `assistant` events; deltas split the text).
    let answers: String = out
        .stdout
        .lines()
        .map(|l| serde_json::from_str::<Value>(l).unwrap())
        .filter(|e| e["type"] == "assistant")
        .map(|e| format!("{}\n", e["text"].as_str().unwrap_or_default()))
        .collect();
    // Turn 1 saw the prompt's stamp, turn 2 the tool result's: the current time on every request.
    assert_current(echoed(&answers, "t1"));
    assert_current(echoed(&answers, "t2"));
    // The tool's own output is what the tool returned (the stamp is not part of it).
    assert!(answers.contains("|r={"), "{answers}");

    // Nothing of the stamp in the output (events, tool results, answer) or the session.
    assert!(!out.stdout.contains("<current_time>"), "{}", out.stdout);
    assert!(!out.stderr.contains("<current_time>"), "{}", out.stderr);
    let session: Value = serde_json::from_str(out.stdout.lines().next().unwrap()).unwrap();
    let file = session["session_file"].as_str().expect("a JSONL session");
    let jsonl = std::fs::read_to_string(file).unwrap();
    assert!(jsonl.contains("what time is it?"), "{jsonl}");
    assert!(!jsonl.contains("<current_time>"), "{jsonl}");
    // The system prompt explains the tag (fixed text, so it stays cached).
    let out = harness(
        &dir,
        &amp.url,
        &["run", "--role", role, "--print-system-prompt"],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(
        out.stdout.contains("\nclock: every user message"),
        "{}",
        out.stdout
    );
}

#[tokio::test]
async fn the_manager_sees_the_current_time_on_every_request_and_nothing_is_kept() {
    check_role("clock-manager", "manager").await;
}

#[tokio::test]
async fn a_worker_sees_the_current_time_on_every_request_and_nothing_is_kept() {
    check_role("clock-worker", "worker-backend").await;
}
