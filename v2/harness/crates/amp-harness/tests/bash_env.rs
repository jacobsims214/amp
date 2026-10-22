//! #2142: the harness's own secrets never reach an agent's bash. A manager and a worker each run
//! `env` through the bash tool (faux model, mock AMP) with dummy values for AMP_TOKEN, the
//! `--token-env` variable and every provider key named in the models config; none of them may
//! show up, everything else must. Only dummy values are used, and failures print variable
//! names, never the `env` output itself.

mod common;

use std::path::Path;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const TOKEN: &str = "dummy-amp-token-2142";
const OPENROUTER: &str = "dummy-openrouter-key-2142";
const ANTHROPIC: &str = "dummy-anthropic-key-2142";
const EXTRA: &str = "dummy-extra-provider-key-2142";
const KEEP: &str = "kept-2142";

const CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

# A provider no role uses: its key is still a secret.
[providers.extra]
api = "openai-completions"
base_url = "http://127.0.0.1:9/v1"
key_env = "DUMMY_EXTRA_PROVIDER_KEY"
[providers.extra.models."m-1"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

/// What each role's bash runs. The manager's policy allows only `git status|diff|log` and `ls`,
/// so it expands the variables into `ls` paths instead: an empty secret leaves `/` (which
/// exists), a set one would show up in the "No such file" error, and the kept variable always
/// does.
fn command(role: &str) -> &'static str {
    if role == "manager" {
        "ls -d \"/${AMP_TOKEN}${OPENROUTER_API_KEY}${ANTHROPIC_API_KEY}${DUMMY_EXTRA_PROVIDER_KEY}\" \"/${KEEP_ME_2142}\""
    } else {
        "env"
    }
}

fn write_script(dir: &Path, role: &str) {
    let script = json!([
        {"tool_calls": [{"name": "bash", "arguments": {"command": command(role)}, "id": "env-1"}]},
        {"text": "done"}
    ]);
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
}

fn setup(name: &str, extra_config: &str) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(
        config.join("config.toml"),
        format!("{CONFIG}\n{extra_config}"),
    )
    .unwrap();
    write_script(&dir, "worker-backend");
    dir
}

async fn run_env(dir: &Path, url: &str, role: &str, args: &[&str], env: &[(&str, &str)]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let mut all = vec![
        ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
        ("XDG_STATE_HOME", state.to_str().unwrap()),
        ("OPENROUTER_API_KEY", OPENROUTER),
        ("ANTHROPIC_API_KEY", ANTHROPIC),
        ("DUMMY_EXTRA_PROVIDER_KEY", EXTRA),
        ("KEEP_ME_2142", KEEP),
    ];
    all.extend_from_slice(env);
    let mut argv = vec![
        "run",
        "-p",
        "run env",
        "--json",
        "--no-session",
        "--role",
        role,
    ];
    argv.extend_from_slice(args);
    run(dir, url, &argv, &all).await
}

/// The bash tool's output and, for `env`, the variable names (values stay out of failure
/// messages). `ok`: the command must have succeeded.
fn env_names(out: &Output, ok: bool) -> (String, Vec<String>) {
    assert_eq!(out.code, 0, "exit {}: {}", out.code, out.stderr);
    let end: Value = out
        .stdout
        .lines()
        .map(|l| serde_json::from_str::<Value>(l).unwrap())
        .find(|e| e["type"] == "tool_end" && e["name"] == "bash")
        .expect("a bash tool_end event");
    if ok {
        assert_eq!(end["is_error"], false, "bash failed");
    }
    let text = end["text"].as_str().unwrap().to_owned();
    let names = text
        .lines()
        .filter_map(|l| l.split_once('=').map(|(k, _)| k.to_owned()))
        .collect();
    (text, names)
}

fn assert_absent(out: &Output, text: &str, names: &[String], secrets: &[(&str, &str)]) {
    for (name, value) in secrets {
        assert!(!names.iter().any(|n| n == name), "{name} reached bash");
        assert!(!text.contains(value), "the value of {name} reached bash");
        assert!(
            !out.stdout.contains(value),
            "the value of {name} is in the run output"
        );
    }
}

#[tokio::test]
async fn manager_and_worker_bash_never_see_amp_token_or_provider_keys() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let dir = setup("bash-env", "");
    for role in ["manager", "worker-backend"] {
        write_script(&dir, role);
        let out = run_env(&dir, &amp.url, role, &[], &[("AMP_TOKEN", TOKEN)]).await;
        let (text, names) = env_names(&out, role != "manager");
        assert_absent(
            &out,
            &text,
            &names,
            &[
                ("AMP_TOKEN", TOKEN),
                ("OPENROUTER_API_KEY", OPENROUTER),
                ("ANTHROPIC_API_KEY", ANTHROPIC),
                ("DUMMY_EXTRA_PROVIDER_KEY", EXTRA),
            ],
        );
        if role == "manager" {
            // The kept variable expanded; the secrets expanded to nothing.
            assert!(text.contains(&format!("/{KEEP}")), "manager: no expansion");
            continue;
        }
        // Everything else is kept.
        assert!(
            text.contains(&format!("KEEP_ME_2142={KEEP}")),
            "{role}: {names:?}"
        );
        assert!(names.iter().any(|n| n == "PATH"), "{role}: {names:?}");
        assert!(
            names.iter().any(|n| n == "AMP_MCP_URL"),
            "{role}: {names:?}"
        );
    }
}

#[tokio::test]
async fn the_token_env_variable_is_removed_and_pass_to_bash_is_honoured() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let dir = setup(
        "bash-env-token-env",
        "[secrets]\npass_to_bash = [\"DUMMY_EXTRA_PROVIDER_KEY\"]\n",
    );
    let out = run_env(
        &dir,
        &amp.url,
        "worker-backend",
        &["--token-env", "CI_AMP_TOKEN_2142"],
        &[
            ("CI_AMP_TOKEN_2142", TOKEN),
            ("AMP_TOKEN", "dummy-unused-2142"),
        ],
    )
    .await;
    let (text, names) = env_names(&out, true);
    assert_absent(
        &out,
        &text,
        &names,
        &[
            ("CI_AMP_TOKEN_2142", TOKEN),
            ("AMP_TOKEN", "dummy-unused-2142"),
            ("OPENROUTER_API_KEY", OPENROUTER),
            ("ANTHROPIC_API_KEY", ANTHROPIC),
        ],
    );
    // Passed through on purpose.
    assert!(
        text.contains(&format!("DUMMY_EXTRA_PROVIDER_KEY={EXTRA}")),
        "{names:?}"
    );
}
