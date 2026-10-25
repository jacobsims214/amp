//! #2163: `amp-harness connect` / `connect --list` / `disconnect` and the chat's `/connect`,
//! without the OS keychain (the file credential store, as on Linux CI). Storing a key then has to
//! fail with the headless way out; everything around it (checks, custom providers, config merges,
//! the hidden prompts) is covered here. The keychain round trips, including the chat picking a new
//! key up without a restart, are in tests/keychain.rs (opt-in, macOS).

mod common;

use std::{path::Path, process::Stdio, time::Duration};

use common::{
    MockAmp, Output,
    provider::{ProviderState, files_containing, spawn_provider},
    spawn, workdir,
};
use serde_json::Value;
use tokio::io::AsyncWriteExt;

const KEY: &str = "sk-mock-2163-never-print-me";
const WRONG: &str = "sk-mock-2163-wrong-key";

/// Runs the binary in `dir/project` with HOME/XDG under `dir/home`, the file credential store,
/// no provider keys in the environment, and `stdin` piped in.
async fn run(dir: &Path, args: &[&str], stdin: Option<&str>, env: &[(&str, &str)]) -> Output {
    let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
    cmd.args(args)
        .current_dir(dir.join("project"))
        .env("HOME", dir.join("home"))
        .env("XDG_CONFIG_HOME", dir.join("home/.config"))
        .env("XDG_STATE_HOME", dir.join("home/.local/state"))
        .env("AMP_CREDENTIAL_STORE", "file")
        .env("AMP_HARNESS_CEILING_DIR", dir)
        .env("AMP_WEB_URL", "http://ui.invalid")
        .env_remove("AMP_MCP_URL")
        .env_remove("AMP_TOKEN")
        .env_remove("AMP_HARNESS_PROFILE")
        .env_remove("AMP_HARNESS_PROMPTS")
        .env_remove("AMP_HARNESS_FAUX_SCRIPT")
        .env_remove("ANTHROPIC_API_KEY")
        .env_remove("OPENROUTER_API_KEY")
        .env_remove("MOCK_KEY")
        .env_remove("RUST_LOG")
        .stdin(if stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (k, v) in env {
        cmd.env(k, v);
    }
    let mut child = cmd.spawn().expect("spawn amp-harness");
    if let Some(text) = stdin {
        let mut pipe = child.stdin.take().unwrap();
        // A run that ends without reading all its input closes the pipe first (#2385).
        if let Err(e) = pipe.write_all(text.as_bytes()).await {
            assert_eq!(e.kind(), std::io::ErrorKind::BrokenPipe, "{e}");
        }
        drop(pipe);
    }
    let out = tokio::time::timeout(Duration::from_secs(60), child.wait_with_output())
        .await
        .expect("amp-harness finished within 60 s")
        .expect("run amp-harness");
    Output {
        code: out.status.code().unwrap_or(-1),
        stdout: String::from_utf8(out.stdout).unwrap(),
        stderr: String::from_utf8(out.stderr).unwrap(),
    }
}

fn config_path(dir: &Path) -> std::path::PathBuf {
    dir.join("home/.config/amp-harness/config.toml")
}

fn write_config(dir: &Path, text: &str) {
    let path = config_path(dir);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

fn read_config(dir: &Path) -> String {
    std::fs::read_to_string(config_path(dir)).unwrap_or_default()
}

fn no_secret(out: &Output, dir: &Path, secrets: &[&str]) {
    for s in secrets {
        assert!(!out.stdout.contains(s), "secret on stdout: {}", out.stdout);
        assert!(!out.stderr.contains(s), "secret on stderr: {}", out.stderr);
        let files = files_containing(dir, s);
        assert!(files.is_empty(), "secret in files: {files:?}");
    }
}

/// The manager on faux (so the chat starts), and a `mock` provider on `url`.
fn mock_config(url: &str) -> String {
    format!(
        "# my own notes\n[server]\n# keep me\n\n[providers.faux]\napi = \"faux\"\n\
         [providers.faux.models.\"faux-1\"]\n\n[roles.manager]\nmodel = \"faux/faux-1\"\n\n\
         [providers.mock]\nname = \"Mock AI\"\napi = \"openai-completions\"\nbase_url = \"{url}\"\n\
         key_env = \"MOCK_KEY\"\n[providers.mock.models.\"m-1\"]\n"
    )
}

#[tokio::test]
async fn list_shows_where_keys_come_from_never_the_key() {
    let dir = workdir("connect-list", None);
    write_config(&dir, &mock_config("http://127.0.0.1:9/v1"));
    let out = run(
        &dir,
        &["connect", "--list", "--json"],
        None,
        &[("OPENROUTER_API_KEY", KEY)],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    no_secret(&out, &dir, &[KEY]);
    let v: Value = out.json();
    let find = |id: &str| {
        v.as_array()
            .unwrap()
            .iter()
            .find(|p| p["id"] == id)
            .cloned()
    };
    let or = find("openrouter").unwrap();
    assert_eq!(or["key"], "env");
    assert_eq!(or["key_present"], true);
    assert_eq!(or["key_source"], "yes (env OPENROUTER_API_KEY)");
    assert_eq!(or["builtin"], true);
    let mock = find("mock").unwrap();
    assert_eq!(mock["name"], "Mock AI");
    assert_eq!(mock["key_present"], false);
    assert_eq!(mock["builtin"], false);
    assert_eq!(find("faux").unwrap()["roles"][0], "manager");

    let human = run(&dir, &["connect", "--list"], None, &[]).await;
    assert_eq!(human.code, 0);
    assert!(human.stdout.contains("PROVIDER"), "{}", human.stdout);
    assert!(
        human.stdout.contains("amp-harness connect [provider]"),
        "{}",
        human.stdout
    );
}

#[tokio::test]
async fn a_rejected_key_or_a_subscription_token_stores_nothing() {
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-rejected", None);
    write_config(&dir, &mock_config(&provider.url));
    let before = read_config(&dir);

    let out = run(
        &dir,
        &["connect", "mock", "--key-stdin"],
        Some(&format!("{WRONG}\n")),
        &[],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(
        out.stderr.contains("rejected the key (HTTP 401)") && out.stderr.contains("nothing"),
        "{}",
        out.stderr
    );
    no_secret(&out, &dir, &[WRONG]);
    assert_eq!(read_config(&dir), before);
    // The check was a model list, with the key as a bearer token.
    assert_eq!(provider.seen()[0].0, "/v1/models");

    // A Claude subscription token is refused before anything is sent.
    let calls = provider.seen().len();
    let oat = "sk-ant-oat01-2163-never-print-me";
    let out = run(
        &dir,
        &["connect", "mock", "--key-stdin"],
        Some(&format!("{oat}\n")),
        &[],
    )
    .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(
        out.stderr.contains("subscription") && out.stderr.contains("console.anthropic.com"),
        "{}",
        out.stderr
    );
    no_secret(&out, &dir, &[oat]);
    assert_eq!(provider.seen().len(), calls);
}

#[tokio::test]
async fn without_a_keychain_the_key_is_checked_but_never_written() {
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-nokeychain", None);
    write_config(&dir, &mock_config(&provider.url));
    let before = read_config(&dir);
    let out = run(
        &dir,
        &["connect", "mock", "--key-stdin"],
        Some(&format!("{KEY}\n")),
        &[],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(out.stderr.contains("accepted the key"), "{}", out.stderr);
    assert!(
        out.stderr
            .contains("set MOCK_KEY in the environment instead"),
        "{}",
        out.stderr
    );
    no_secret(&out, &dir, &[KEY]);
    assert_eq!(read_config(&dir), before);
}

/// #2340: with the keychain off, a terminal asks for no key it could not keep: the picker, then
/// the variable to set (no hidden prompt). The hidden prompt itself is covered with a keychain in
/// tests/keychain.rs.
#[tokio::test]
async fn without_a_keychain_the_terminal_asks_for_no_key() {
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-scripted", None);
    write_config(&dir, &mock_config(&provider.url));
    let before = read_config(&dir);
    // The picker (openrouter, anthropic, openai, then faux? no: faux is hidden, then mock). A
    // key line after it is never read as an answer.
    let out = run(
        &dir,
        &["connect"],
        Some(&format!("4\n{KEY}\n")),
        &[("AMP_HARNESS_PROMPTS", "stdin")],
    )
    .await;
    assert!(
        out.stderr.contains("4) Mock AI"),
        "the picker lists the configured provider: {}",
        out.stderr
    );
    assert!(out.stderr.contains("5) Custom"), "{}", out.stderr);
    assert!(!out.stderr.contains("(hidden)"), "{}", out.stderr);
    assert!(
        out.stderr.contains(
            "the OS keychain is turned off (AMP_CREDENTIAL_STORE=file), so a key typed here for \
             Mock AI could not be kept: set MOCK_KEY in your environment"
        ),
        "{}",
        out.stderr
    );
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(provider.seen().is_empty(), "{:?}", provider.seen());
    no_secret(&out, &dir, &[KEY]);
    assert_eq!(read_config(&dir), before);
}

/// #2340: in a terminal, a key in the provider's variable is used as it is without a terminal:
/// checked, left in the environment, no hidden prompt.
#[tokio::test]
async fn a_terminal_uses_the_key_in_the_environment() {
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-scripted-env", None);
    write_config(&dir, &mock_config(&provider.url));
    let out = run(
        &dir,
        &["connect", "mock"],
        // "Run … on mock/m-1?": no.
        Some("n\n"),
        &[("AMP_HARNESS_PROMPTS", "stdin"), ("MOCK_KEY", KEY)],
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(!out.stderr.contains("(hidden)"), "{}", out.stderr);
    assert!(
        out.stderr.contains(
            "Using the Mock AI key in MOCK_KEY (it stays in the environment; nothing is stored)."
        ),
        "{}",
        out.stderr
    );
    assert!(out.stderr.contains("accepted the key"), "{}", out.stderr);
    no_secret(&out, &dir, &[KEY]);
}

#[tokio::test]
async fn a_custom_provider_round_trip() {
    let provider = spawn_provider(ProviderState {
        models: vec!["llama-4".into(), "qwen-3".into()],
        reply: "hi".into(),
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-custom", None);
    write_config(&dir, &mock_config("http://127.0.0.1:9/v1"));
    let before = read_config(&dir);

    // Secret-looking headers never go on the command line.
    let out = run(
        &dir,
        &[
            "connect",
            "local",
            "--base-url",
            &provider.url,
            "--header",
            "Authorization=Bearer sk-x",
            "--key-stdin",
        ],
        Some(""),
        &[],
    )
    .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("looks like a secret"), "{}", out.stderr);

    // An unknown id without --base-url says how to add it.
    let out = run(&dir, &["connect", "nope", "--key-stdin"], Some(""), &[]).await;
    assert_eq!(out.code, 2);
    assert!(out.stderr.contains("--base-url"), "{}", out.stderr);

    // A keyless local endpoint (no key on stdin): no keychain needed.
    let out = run(
        &dir,
        &[
            "connect",
            "local",
            "--base-url",
            &format!("{}/", provider.url),
            "--name",
            "Local LLM",
            "--header",
            "X-Title=AMP",
            "--key-stdin",
            "--use",
            "--json",
        ],
        Some(""),
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let v = out.json();
    assert_eq!(v["provider"], "local");
    assert_eq!(v["key"], "none");
    assert_eq!(v["models_added"], serde_json::json!(["llama-4", "qwen-3"]));
    assert_eq!(v["selection"], "inherit local/llama-4");
    let backup = v["backup"].as_str().unwrap();
    assert_eq!(std::fs::read_to_string(backup).unwrap(), before);

    // Merged: the person's comments and tables are still there; the new table loads.
    let text = read_config(&dir);
    assert!(
        text.starts_with("# my own notes\n[server]\n# keep me\n"),
        "{text}"
    );
    assert!(text.contains("[providers.mock]") && text.contains("[providers.local]"));
    assert!(text.contains("auth = \"none\"") && text.contains("\"X-Title\" = \"AMP\""));
    assert!(
        text.contains(&format!("base_url = \"{}\"", provider.url)),
        "{text}"
    );
    let show = run(&dir, &["config", "show", "--json"], None, &[]).await;
    let show = show.json();
    let manager = show["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == "manager")
        .unwrap()
        .clone();
    // The person's own [roles.manager] still wins over inherit.
    assert_eq!(manager["provider"], "faux");
    let worker = show["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == "worker-backend")
        .unwrap()
        .clone();
    assert_eq!(worker["provider"], "local");
    assert_eq!(worker["model"], "llama-4");
    assert_eq!(worker["ready"], true);

    // A headless run on it: the role really talks to the endpoint.
    let amp = spawn(MockAmp::new(), None).await;
    let out = run(
        &dir,
        &["run", "-p", "say hi", "--role", "worker-docs", "--json"],
        None,
        &[("AMP_MCP_URL", &amp.url)],
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(
        provider
            .seen()
            .iter()
            .any(|(path, _, _)| path == "/v1/chat/completions"),
        "{:?}",
        provider.seen()
    );

    // Roles still use it: its table stays (removing it would break the config).
    let out = run(&dir, &["disconnect", "local"], None, &[]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(
        out.stderr.contains("roles still run on local"),
        "{}",
        out.stderr
    );
    assert!(read_config(&dir).contains("[providers.local]"));

    // Point the roles elsewhere, then remove it.
    let text = read_config(&dir).replace("profile = \"inherit\"", "profile = \"opencode\"");
    std::fs::write(config_path(&dir), &text).unwrap();
    let out = run(&dir, &["disconnect", "local", "--json"], None, &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let v = out.json();
    assert_eq!(v["config_removed"], true);
    let after = read_config(&dir);
    assert!(!after.contains("[providers.local"), "{after}");
    assert!(after.contains("[providers.mock]") && after.starts_with("# my own notes"));
    assert!(!after.contains("Added by"), "no leftover comment: {after}");

    // A table written by hand is never removed.
    let out = run(&dir, &["disconnect", "mock"], None, &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(
        out.stdout.contains("written by hand, so it stays"),
        "{}",
        out.stdout
    );
    assert_eq!(read_config(&dir), after);
}

#[tokio::test]
async fn the_chat_connect_command_asks_hidden_and_lists() {
    let amp = spawn(MockAmp::new(), None).await;
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("connect-chat", None);
    write_config(&dir, &mock_config(&provider.url));
    let paste = format!("key:paste:{KEY}");
    let out = run(
        &dir,
        &[
            "--snapshot",
            "160x40",
            "--no-session",
            "--input",
            "/connect",
            "--input",
            "/connect nope",
            "--input",
            "/connect mock",
            "--input",
            &paste,
            "--input",
            "key:enter",
        ],
        None,
        &[("AMP_MCP_URL", &amp.url)],
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let screen = &out.stdout;
    assert!(
        screen.contains("PROVIDER") && screen.contains("Mock AI"),
        "{screen}"
    );
    assert!(screen.contains("unknown provider `nope`"), "{screen}");
    assert!(
        screen.contains("Mock AI API key: paste or type it now"),
        "{screen}"
    );
    assert!(
        screen.contains("accepted the key") || screen.contains("keychain"),
        "{screen}"
    );
    // No keychain here: the store fails and says how to set the key instead.
    assert!(screen.contains("MOCK_KEY"), "{screen}");
    no_secret(&out, &dir, &[KEY]);
}
