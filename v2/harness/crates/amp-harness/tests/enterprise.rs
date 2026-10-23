//! #2275: the enterprise path. A company's own OpenAI-compatible endpoint (the in-crate mock
//! provider, serving models `engineer` and `orchestrator`), set up without editing a file:
//!
//! - the first run's "your own OpenAI-compatible endpoint" choice, which lists the models and
//!   maps them to the roles by name;
//! - `connect <id> --role ROLE=MODEL` and the chat's `/connect <id> role=model`;
//! - `setup --preset FILE|URL` with `examples/enterprise-preset.toml`;
//! - `doctor` naming the provider, its endpoint and each role's model.
//!
//! Every run goes through a recording proxy guard (`HTTPS_PROXY`, with `NO_PROXY` for 127.0.0.1),
//! so a request to openrouter.ai (or anywhere off this machine) would be recorded: the tests
//! assert there was none. HOME and config live under cargo's target dir; no keychain is used
//! (`AMP_CREDENTIAL_STORE=file`), keys come from the environment.

mod common;

use std::{path::Path, process::Stdio, time::Duration};

use common::{
    MockAmp, Output,
    provider::{
        MockProvider, ProviderState, ProxyGuard, files_containing, spawn_provider,
        spawn_proxy_guard,
    },
    spawn, workdir,
};
use serde_json::Value;
use tokio::io::AsyncWriteExt;

/// A key-shaped value that must never show up in output or files.
const KEY: &str = "sk-example-2275-never-print-me";

const EXAMPLE_PRESET: &str = include_str!("../../../examples/enterprise-preset.toml");

struct Env<'a> {
    dir: &'a Path,
    guard: &'a ProxyGuard,
    vars: Vec<(&'a str, String)>,
}

impl<'a> Env<'a> {
    fn new(dir: &'a Path, guard: &'a ProxyGuard) -> Self {
        Self {
            dir,
            guard,
            vars: Vec::new(),
        }
    }

    fn var(mut self, k: &'a str, v: impl Into<String>) -> Self {
        self.vars.push((k, v.into()));
        self
    }

    /// Runs the binary in `dir/project` with HOME/XDG under `dir/home`, no keychain, no provider
    /// keys or AMP settings from this machine, every off-machine request sent to the guard.
    async fn run(&self, args: &[&str], stdin: Option<&str>) -> Output {
        let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(args)
            .current_dir(self.dir.join("project"))
            .env("HOME", self.dir.join("home"))
            .env("XDG_CONFIG_HOME", self.dir.join("home/.config"))
            .env("XDG_STATE_HOME", self.dir.join("home/.local/state"))
            .env("AMP_CREDENTIAL_STORE", "file")
            .env("AMP_HARNESS_CEILING_DIR", self.dir)
            .env("AMP_WEB_URL", "http://ui.invalid")
            .env_remove("AMP_MCP_URL")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_PROMPTS")
            .env_remove("AMP_HARNESS_FAUX_SCRIPT")
            .env_remove("AMP_HARNESS_CONFIG")
            .env_remove("AMP_HARNESS_CONFIG_DIR")
            .env_remove("ANTHROPIC_API_KEY")
            .env_remove("OPENROUTER_API_KEY")
            .env_remove("OPENAI_API_KEY")
            .env_remove("EXAMPLE_API_KEY")
            .env_remove("RUST_LOG")
            .stdin(if stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for (k, v) in self.guard.env() {
            cmd.env(k, v);
        }
        for (k, v) in &self.vars {
            cmd.env(k, v);
        }
        let mut child = cmd.spawn().expect("spawn amp-harness");
        if let Some(text) = stdin {
            let mut pipe = child.stdin.take().unwrap();
            pipe.write_all(text.as_bytes()).await.unwrap();
            drop(pipe);
        }
        let out = tokio::time::timeout(Duration::from_secs(90), child.wait_with_output())
            .await
            .expect("amp-harness finished within 90 s")
            .expect("run amp-harness");
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr: String::from_utf8(out.stderr).unwrap(),
        }
    }
}

fn config(dir: &Path) -> String {
    std::fs::read_to_string(dir.join("home/.config/amp-harness/config.toml")).unwrap_or_default()
}

fn write_config(dir: &Path, text: &str) {
    let path = dir.join("home/.config/amp-harness/config.toml");
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

/// The company endpoint: models `engineer` and `orchestrator`, optionally behind a key.
async fn example_endpoint(key: Option<&str>) -> MockProvider {
    spawn_provider(ProviderState {
        key: key.map(str::to_owned),
        models: vec!["engineer".into(), "orchestrator".into()],
        reply: "on it".into(),
        ..ProviderState::default()
    })
    .await
}

/// Nothing left this machine: no request to openrouter.ai, nor anywhere else.
fn assert_nothing_left_the_machine(guard: &ProxyGuard) {
    let seen = guard.seen();
    assert!(
        !seen.iter().any(|l| l.contains("openrouter")),
        "a request went to OpenRouter: {seen:?}"
    );
    assert!(seen.is_empty(), "requests left the machine: {seen:?}");
}

fn no_secret(out: &Output, dir: &Path) {
    assert!(!out.stdout.contains(KEY), "key on stdout: {}", out.stdout);
    assert!(!out.stderr.contains(KEY), "key on stderr: {}", out.stderr);
    let files = files_containing(dir, KEY);
    assert!(files.is_empty(), "key in files: {files:?}");
}

/// `models list --json`: role -> `provider/model`.
async fn role_models(env: &Env<'_>) -> std::collections::BTreeMap<String, String> {
    let out = env.run(&["models", "list", "--json"], None).await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let v = out.json();
    let rows = v["roles"]
        .as_array()
        .or_else(|| v.as_array())
        .unwrap_or_else(|| panic!("no roles in {v}"))
        .clone();
    rows.iter()
        .map(|r| {
            (
                r["role"].as_str().unwrap().to_owned(),
                format!(
                    "{}/{}",
                    r["provider"].as_str().unwrap(),
                    r["model"].as_str().unwrap()
                ),
            )
        })
        .collect()
}

fn assert_mapping(roles: &std::collections::BTreeMap<String, String>, reviewers: &str) {
    assert_eq!(roles["manager"], "example/orchestrator", "{roles:?}");
    for r in [
        "worker-backend",
        "worker-frontend",
        "worker-docs",
        "designer",
        "researcher",
        "media",
        "github-researcher",
        "kb-curator",
    ] {
        assert_eq!(roles[r], "example/engineer", "{r}: {roles:?}");
    }
    for r in ["reviewer", "github-reviewer"] {
        assert_eq!(roles[r], reviewers, "{r}: {roles:?}");
    }
    assert!(
        roles.values().all(|m| m.starts_with("example/")),
        "every role on the endpoint: {roles:?}"
    );
}

/// The first run: no provider key anywhere, so the provider menu; "your own endpoint" with a
/// bare host:port, no key, the listed models mapped by name. Then a headless run really uses
/// `orchestrator` for the manager, and doctor names the endpoint and each role's model.
#[tokio::test]
async fn the_first_run_sets_up_an_own_endpoint_and_maps_models_to_roles() {
    let guard = spawn_proxy_guard().await;
    let amp = spawn(MockAmp::new(), None).await;
    let endpoint = example_endpoint(None).await;
    let dir = workdir("enterprise-first-run", None);
    let env = Env::new(&dir, &guard)
        .var("AMP_HARNESS_PROMPTS", "stdin")
        .var("AMP_MCP_URL", amp.url.clone());
    let host = endpoint
        .url
        .trim_start_matches("http://")
        .trim_end_matches("/v1")
        .to_owned();
    // No project for now; provider 4 (own endpoint); name; id; the address (bare host:port),
    // confirm the /v1 URL; no extra header; no key; keep the suggested mapping.
    let answers = format!("\n4\nExample AI\nexample\n{host}\n\n\n\n\n");
    let out = env
        .run(&["--snapshot", "100x20", "--no-session"], Some(&answers))
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let err = &out.stderr;
    for want in [
        "Your own OpenAI-compatible endpoint",
        &format!("Use {}?", endpoint.url),
        "It serves 2 models: engineer, orchestrator.",
        "manager    example/orchestrator",
        "workers    example/engineer",
    ] {
        assert!(err.contains(want), "missing {want:?}:\n{err}");
    }
    assert!(!err.contains("OpenRouter API key"), "{err}");

    let text = config(&dir);
    assert!(text.contains("[providers.example]"), "{text}");
    assert!(
        text.contains(&format!("base_url = \"{}\"", endpoint.url)),
        "{text}"
    );
    assert!(text.contains("auth = \"none\""), "{text}");
    assert!(text.contains("profile = \"inherit\""), "{text}");
    assert!(text.contains("inherit = \"example/engineer\""), "{text}");
    assert_mapping(&role_models(&env).await, "example/orchestrator");

    // The manager runs on orchestrator, against the endpoint.
    let out = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert_eq!(endpoint.requested_models(), vec!["orchestrator".to_owned()]);

    // doctor: the endpoint answers /models; provider, endpoint and each role's model are shown.
    let out = env.run(&["doctor", "--json"], None).await;
    let v = out.json();
    let check = |name: &str| {
        v["checks"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["name"] == name)
            .unwrap_or_else(|| panic!("no check {name}: {v}"))
            .clone()
    };
    assert_eq!(check("models:example")["status"], "ok", "{v}");
    assert_eq!(check("key:example")["status"], "ok", "{v}");
    assert!(
        v["checks"]
            .as_array()
            .unwrap()
            .iter()
            .all(|c| !c["name"].as_str().unwrap().ends_with(":openrouter")),
        "OpenRouter is not in use: {v}"
    );
    let manager = v["models"]["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == "manager")
        .unwrap()
        .clone();
    assert_eq!(manager["model"], "orchestrator");
    assert_eq!(manager["endpoint"], endpoint.url.as_str());
    let human = env.run(&["doctor"], None).await;
    assert!(
        human
            .stdout
            .contains(&format!("provider: example (Example AI) {}", endpoint.url)),
        "{}",
        human.stdout
    );
    assert!(
        human
            .stdout
            .lines()
            .any(|l| l.contains(" manager ") && l.trim_end().ends_with("example/orchestrator")),
        "{}",
        human.stdout
    );

    // A second start asks for nothing but the project (still skipped).
    let out = env
        .run(&["--snapshot", "100x20", "--no-session"], Some("\n"))
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert_nothing_left_the_machine(&guard);
}

/// `connect example --base-url api-host --role manager=orchestrator --role worker=engineer`: the
/// key from the environment, checked against /models; the roles mapped in one step; then
/// `connect example --role reviewers=orchestrator` changes only the roles (no key asked), and bad
/// `--role`s are refused before anything is written.
#[tokio::test]
async fn connect_maps_models_per_role() {
    let guard = spawn_proxy_guard().await;
    let endpoint = example_endpoint(Some(KEY)).await;
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("enterprise-connect-role", None);
    let env = Env::new(&dir, &guard)
        .var("EXAMPLE_API_KEY", KEY)
        .var("AMP_MCP_URL", amp.url.clone());
    let host = endpoint.url.trim_end_matches("/v1").to_owned();

    // A bad role or model: refused, nothing written.
    for (role, want) in [
        ("boss=engineer", "unknown role `boss`"),
        ("manager=gpt-9", "example has no model `gpt-9`"),
    ] {
        let out = env
            .run(
                &["connect", "example", "--base-url", &host, "--role", role],
                None,
            )
            .await;
        assert_ne!(out.code, 0, "{role}: {}", out.stderr);
        assert!(out.stderr.contains(want), "{role}: {}", out.stderr);
        assert!(!config(&dir).contains("[providers.example]"), "{role}");
    }

    // No terminal, no --key-stdin: the key in EXAMPLE_API_KEY is checked and stays there.
    let out = env
        .run(
            &[
                "connect",
                "example",
                "--base-url",
                &host,
                "--name",
                "Example AI",
                "--role",
                "manager=orchestrator",
                "--role",
                "worker=engineer",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(
        out.stderr
            .contains("Using the Example AI key in EXAMPLE_API_KEY"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr.contains("Example AI accepted the key"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr.contains("manager    example/orchestrator"),
        "{}",
        out.stderr
    );
    no_secret(&out, &dir);
    assert!(
        config(&dir).contains(&format!("base_url = \"{}\"", endpoint.url)),
        "{}",
        config(&dir)
    );
    assert_mapping(&role_models(&env).await, "example/engineer");

    // Only the roles: it has its key (env), so nothing is asked.
    let out = env
        .run(
            &["connect", "example", "--role", "reviewers=orchestrator"],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(out.stdout.contains("mapped the roles"), "{}", out.stdout);
    assert_mapping(&role_models(&env).await, "example/orchestrator");

    let out = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert_eq!(endpoint.requested_models(), vec!["orchestrator".to_owned()]);
    assert!(
        endpoint
            .seen()
            .iter()
            .all(|(_, auth, _)| auth.as_deref() == Some(&format!("Bearer {KEY}") as &str)),
        "{:?}",
        endpoint.seen()
    );
    no_secret(&out, &dir);
    assert_nothing_left_the_machine(&guard);
}

/// The chat's `/connect example manager=orchestrator worker=engineer` on a provider that needs no
/// key: nothing is asked, the roles are written.
#[tokio::test]
async fn the_chat_connect_maps_roles() {
    let guard = spawn_proxy_guard().await;
    let amp = spawn(MockAmp::new(), None).await;
    let endpoint = example_endpoint(None).await;
    let dir = workdir("enterprise-chat-connect", None);
    write_config(
        &dir,
        &format!(
            "[models]\nprofile = \"inherit\"\ninherit = \"example/engineer\"\n\n\
             [providers.example]\nname = \"Example AI\"\napi = \"openai-completions\"\n\
             base_url = \"{}\"\nauth = \"none\"\n[providers.example.models.engineer]\n\
             [providers.example.models.orchestrator]\n",
            endpoint.url
        ),
    );
    let env = Env::new(&dir, &guard).var("AMP_MCP_URL", amp.url.clone());
    let out = env
        .run(
            &[
                "--snapshot",
                "160x40",
                "--no-session",
                "--input",
                "/connect example manager=orchestrator --role reviewers=orchestrator",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(
        out.stdout.contains("The roles now run on"),
        "{}",
        out.stdout
    );
    assert!(
        !out.stdout.contains("paste or type it now"),
        "{}",
        out.stdout
    );
    assert_mapping(&role_models(&env).await, "example/orchestrator");
    assert_nothing_left_the_machine(&guard);
}

/// The shipped example preset, pointed at the mock endpoint and mock AMP (only its two URLs are
/// changed), applied with `setup --preset FILE --yes`: the server, the provider and the roles come
/// from it; the person's key from the environment; doctor and a headless run use the endpoint.
#[tokio::test]
async fn the_example_preset_works_end_to_end() {
    let guard = spawn_proxy_guard().await;
    let amp = spawn(MockAmp::new(), None).await;
    let endpoint = example_endpoint(Some(KEY)).await;
    let dir = workdir("enterprise-preset-file", None);
    assert!(EXAMPLE_PRESET.contains("https://api.example.com/v1"));
    assert!(EXAMPLE_PRESET.contains("models = [\"engineer\", \"orchestrator\"]"));
    let preset = EXAMPLE_PRESET
        .replace("https://api.example.com/v1", &endpoint.url)
        .replace("https://amp.example.com", &amp.url);
    let file = dir.join("enterprise-preset.toml");
    std::fs::write(&file, &preset).unwrap();
    let env = Env::new(&dir, &guard).var("EXAMPLE_API_KEY", KEY);

    let out = env
        .run(
            &[
                "setup",
                "--preset",
                file.to_str().unwrap(),
                "--yes",
                "--project",
                "2",
                "--json",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    no_secret(&out, &dir);
    let v = out.json();
    assert_eq!(v["preset"]["name"], "Example Corp");
    assert_eq!(v["preset"]["providers"], serde_json::json!(["example"]));
    assert_eq!(v["preset"]["roles"]["manager"], "example/orchestrator");
    assert_eq!(v["server"], amp.url.as_str());
    assert_eq!(v["project"]["id"], 2);
    assert_eq!(v["providers"]["example"], "env EXAMPLE_API_KEY");
    assert!(v["providers"].get("openrouter").is_none(), "{v}");
    assert!(
        out.stderr
            .contains("Applied the org preset \"Example Corp\""),
        "{}",
        out.stderr
    );

    let text = config(&dir);
    assert!(
        text.contains("From the org preset \"Example Corp\""),
        "{text}"
    );
    assert!(text.contains(&amp.url), "{text}");
    assert_mapping(&role_models(&env).await, "example/orchestrator");

    let out = env.run(&["doctor", "--json"], None).await;
    let v = out.json();
    let status = |name: &str| {
        v["checks"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["name"] == name)
            .map(|c| c["status"].as_str().unwrap().to_owned())
    };
    assert_eq!(status("models:example").as_deref(), Some("ok"), "{v}");
    assert_eq!(status("key:example").as_deref(), Some("ok"), "{v}");
    assert_eq!(status("key:openrouter"), None, "{v}");
    no_secret(&out, &dir);

    // Doctor's /models call carried the person's key.
    assert!(
        endpoint
            .seen()
            .iter()
            .any(|(path, auth, _)| path == "/v1/models"
                && auth.as_deref() == Some(&format!("Bearer {KEY}") as &str)),
        "{:?}",
        endpoint.seen()
    );
    let out = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert_eq!(endpoint.requested_models(), vec!["orchestrator".to_owned()]);
    assert_nothing_left_the_machine(&guard);
}

/// `setup --preset http://127.0.0.1:N/…` (http is allowed for this machine only); a remote http
/// URL and a preset with a secret are refused before anything is written.
#[tokio::test]
async fn a_preset_from_a_url_and_bad_presets() {
    use axum::{Router, routing::get};
    let guard = spawn_proxy_guard().await;
    let endpoint = example_endpoint(None).await;
    let dir = workdir("enterprise-preset-url", None);
    let env = Env::new(&dir, &guard);

    let good = format!(
        "[preset]\nname = \"Local\"\n[providers.example]\nbase_url = \"{}\"\nauth = \"none\"\n\
         models = [\"engineer\", \"orchestrator\"]\n[roles]\ndefault = \"engineer\"\n\
         manager = \"orchestrator\"\nreviewers = \"orchestrator\"\n\
         [mcp.docs]\ntype = \"http\"\nurl = \"https://mcp.example.com/mcp\"\n",
        endpoint.url
    );
    let secret = format!(
        "[providers.example]\nbase_url = \"{}\"\nmodels = [\"engineer\"]\napi_key = \"{KEY}\"\n",
        endpoint.url
    );
    let app = Router::new()
        .route("/good.toml", get(move || async move { good }))
        .route("/secret.toml", get(move || async move { secret }));
    let tcp = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = tcp.local_addr().unwrap();
    let server = tokio::spawn(async move {
        let _ = axum::serve(tcp, app).await;
    });

    // Refused: plain http off this machine (not fetched), and a preset holding a key.
    let out = env
        .run(
            &[
                "setup",
                "--preset",
                "http://presets.example.com/p.toml",
                "--yes",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("only https:// URLs"), "{}", out.stderr);
    let out = env
        .run(
            &[
                "setup",
                "--preset",
                &format!("http://{addr}/secret.toml"),
                "--yes",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(
        out.stderr.contains("`providers.example.api_key`")
            && out.stderr.contains("never carry secrets"),
        "{}",
        out.stderr
    );
    no_secret(&out, &dir);
    assert!(config(&dir).is_empty(), "nothing written: {}", config(&dir));

    // The good one, without a server in it: --url gives the AMP server.
    let amp = spawn(MockAmp::new(), None).await;
    let out = env
        .run(
            &[
                "setup",
                "--preset",
                &format!("http://{addr}/good.toml"),
                "--url",
                &amp.url,
                "--yes",
                "--no-project",
                "--json",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let v: Value = out.json();
    assert_eq!(v["preset"]["mcp"], serde_json::json!(["docs"]));
    assert_eq!(v["providers"]["example"], "not needed");
    let text = config(&dir);
    assert!(text.contains("[mcp.docs]"), "{text}");
    assert_mapping(&role_models(&env).await, "example/orchestrator");
    let out = env.run(&["config", "check"], None).await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    server.abort();
    assert_nothing_left_the_machine(&guard);
}

/// The guard is real: with the shipped roster and an OpenRouter key, doctor's catalog check goes
/// to openrouter.ai, and the guard records it (so the assertions above can fail).
#[tokio::test]
async fn the_guard_records_a_request_to_openrouter() {
    let guard = spawn_proxy_guard().await;
    let dir = workdir("enterprise-guard", None);
    let env = Env::new(&dir, &guard).var("OPENROUTER_API_KEY", KEY);
    let out = env.run(&["doctor", "--json"], None).await;
    assert!(!out.stdout.is_empty(), "{}", out.stderr);
    let seen = guard.seen();
    assert!(
        seen.iter().any(|l| l.contains("openrouter.ai")),
        "the guard saw: {seen:?}"
    );
    no_secret(&out, &dir);
}
