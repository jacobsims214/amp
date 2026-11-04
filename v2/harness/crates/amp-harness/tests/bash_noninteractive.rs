//! #2143: an agent's bash never waits on a person. A worker (faux model, mock AMP) runs commands
//! that would prompt or hang: `git commit` with no `-m` in a throwaway repo under target/ (opens
//! $EDITOR), a read from /dev/tty and a `sleep` with no timeout. Each must fail fast, through the
//! non-interactive environment or the default bash timeout, while a model-supplied timeout still
//! wins. Every run is bounded by the common `run` helper (60 s) and by the asserts on wall time.

mod common;

use std::{
    path::Path,
    time::{Duration, Instant},
};

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const TOKEN: &str = "dummy-amp-token-2143";

const CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.worker-backend]
model = "faux/faux-1"
"#;

/// A throwaway repo with an identity and no signing, so only the missing message can stop
/// `git commit`.
fn git_repo(dir: &Path) {
    let repo = dir.join("project/repo");
    std::fs::create_dir_all(&repo).unwrap();
    for args in [
        &["init", "-q"][..],
        &["config", "user.name", "AMP Test"],
        &["config", "user.email", "test@example.invalid"],
        &["config", "commit.gpgsign", "false"],
    ] {
        let st = std::process::Command::new("git")
            .args(args)
            .current_dir(&repo)
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("HOME", dir.join("home"))
            .status()
            .unwrap();
        assert!(st.success(), "git {args:?}");
    }
}

fn setup(name: &str, extra_config: &str, steps: Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(
        config.join("config.toml"),
        format!("{CONFIG}\n{extra_config}"),
    )
    .unwrap();
    std::fs::write(dir.join("script.json"), steps.to_string()).unwrap();
    dir
}

async fn run_worker(dir: &Path, url: &str, env: &[(&str, &str)]) -> (Output, Duration) {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let mut all = vec![
        ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
        ("XDG_STATE_HOME", state.to_str().unwrap()),
        ("AMP_TOKEN", TOKEN),
        ("GIT_CONFIG_NOSYSTEM", "1"),
        // A real editor in the harness's own environment must not reach bash.
        ("EDITOR", "vi"),
        ("VISUAL", "vi"),
        ("GIT_EDITOR", "vi"),
    ];
    all.extend_from_slice(env);
    let cwd = dir.join("project");
    let argv = [
        "run",
        "-p",
        "run the commands",
        "--json",
        "--no-session",
        "--role",
        "worker-backend",
        "--cwd",
        cwd.to_str().unwrap(),
    ];
    let started = Instant::now();
    let out = run(dir, url, &argv, &all).await;
    (out, started.elapsed())
}

fn bash(id: &str, command: &str, timeout: Option<f64>) -> Value {
    let mut args = json!({"command": command});
    if let Some(t) = timeout {
        args["timeout"] = json!(t);
    }
    json!({"tool_calls": [{"name": "bash", "arguments": args, "id": id}]})
}

/// The `tool_end` event of call `id`.
fn tool_end(out: &Output, id: &str) -> Value {
    assert_eq!(out.code, 0, "exit {}: {}", out.code, out.stderr);
    out.stdout
        .lines()
        .map(|l| serde_json::from_str::<Value>(l).unwrap())
        .find(|e| e["type"] == "tool_end" && e["id"] == id)
        .unwrap_or_else(|| panic!("no tool_end for {id}: {}", out.stderr))
}

fn text(end: &Value) -> &str {
    end["text"].as_str().unwrap_or_default()
}

#[tokio::test]
async fn prompting_commands_fail_fast_and_the_default_timeout_ends_a_hang() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let steps = json!([
        bash("commit", "cd repo && git commit --allow-empty", None),
        // Under a terminal this is stopped (SIGTTIN) until the timeout; without one it fails.
        bash("tty", "read -r line < /dev/tty", None),
        bash("hang", "sleep 30", None),
        // The model's own timeout wins over the 2 s default.
        bash("longer", "sleep 3 && echo slept", Some(20.0)),
        bash("env", "env", None),
        {"text": "done"}
    ]);
    let dir = setup("bash-noninteractive", "", steps);
    git_repo(&dir);
    let (out, elapsed) =
        run_worker(&dir, &amp.url, &[("AMP_HARNESS_BASH_TIMEOUT_SECS", "2")]).await;

    let commit = tool_end(&out, "commit");
    assert_eq!(commit["is_error"], true, "git commit: {}", text(&commit));
    assert!(
        text(&commit).contains("empty commit message"),
        "git commit did not abort on the empty message: {}",
        text(&commit)
    );

    let tty = tool_end(&out, "tty");
    assert_eq!(tty["is_error"], true, "/dev/tty read: {}", text(&tty));

    let hang = tool_end(&out, "hang");
    assert_eq!(hang["is_error"], true, "sleep 30: {}", text(&hang));
    assert!(
        text(&hang).contains("timed out"),
        "sleep 30: {}",
        text(&hang)
    );

    let longer = tool_end(&out, "longer");
    assert_eq!(
        longer["is_error"],
        false,
        "model timeout: {}",
        text(&longer)
    );
    assert!(text(&longer).contains("slept"));

    let env = text(&tool_end(&out, "env")).to_owned();
    for kv in [
        "GIT_TERMINAL_PROMPT=0",
        "GIT_EDITOR=true",
        "EDITOR=true",
        "VISUAL=true",
        "DEBIAN_FRONTEND=noninteractive",
        "npm_config_yes=true",
        "GIT_SSH_COMMAND=ssh -o BatchMode=yes",
    ] {
        assert!(env.lines().any(|l| l == kv), "{kv} not in bash env");
    }
    // #2142's scrubbing still applies.
    assert!(!env.contains(TOKEN), "AMP_TOKEN reached bash");

    // commit + tty (<= 2 s) + hang (2 s) + longer (3 s), nowhere near 30 s or a hang.
    assert!(elapsed < Duration::from_secs(25), "took {elapsed:?}");
}

#[tokio::test]
async fn config_sets_the_default_timeout() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let steps = json!([bash("hang", "sleep 30", None), {"text": "done"}]);
    let dir = setup(
        "bash-timeout-config",
        "[tools]\nbash_timeout_secs = 1\n",
        steps,
    );
    let (out, elapsed) = run_worker(&dir, &amp.url, &[]).await;
    let hang = tool_end(&out, "hang");
    assert_eq!(hang["is_error"], true, "sleep 30: {}", text(&hang));
    assert!(text(&hang).contains("timed out"), "{}", text(&hang));
    assert!(elapsed < Duration::from_secs(20), "took {elapsed:?}");
}

#[tokio::test]
async fn a_bad_timeout_is_an_error_naming_its_source() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let dir = setup("bash-timeout-bad", "", json!([{"text": "done"}]));
    let (out, _) = run_worker(&dir, &amp.url, &[("AMP_HARNESS_BASH_TIMEOUT_SECS", "soon")]).await;
    assert_ne!(out.code, 0);
    assert!(
        out.stderr.contains("AMP_HARNESS_BASH_TIMEOUT_SECS"),
        "{}",
        out.stderr
    );
}
