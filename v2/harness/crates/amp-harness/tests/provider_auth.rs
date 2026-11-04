//! #2277: provider endpoint auth from the person's side, against the in-crate mock provider.
//!
//! - `connect` with `--auth header` (Azure's `api-key`), `--query api-version=…`,
//!   `--organization` / `--project`, then a headless run and doctor;
//! - `connect` with `--auth command` (a token helper), then a run and doctor's token expiry;
//! - the first run's own-endpoint path answering "How does your endpoint authenticate?" with
//!   Microsoft Entra ID through the Azure CLI (a fake `az` on PATH).
//!
//! HOME/XDG live under cargo's target dir, no keychain is used (`AMP_CREDENTIAL_STORE=file`), every
//! off-machine request goes to a recording proxy guard, and each planted key or token is asserted
//! absent from stdout, stderr and every file.

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

const KEY: &str = "sk-gw-2277-header-never-print";
const COMMAND_TOKEN: &str = "tok-2277-command-planted-never-print";
const AZ_TOKEN: &str = "az-2277-entra-planted-never-print";

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
            // A run that ends without reading all its input closes the pipe first (#2385).
            if let Err(e) = pipe.write_all(text.as_bytes()).await {
                assert_eq!(e.kind(), std::io::ErrorKind::BrokenPipe, "{e}");
            }
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

fn no_secret(secret: &str, outs: &[&Output], dir: &Path) {
    for out in outs {
        assert!(
            !out.stdout.contains(secret),
            "secret on stdout: {}",
            out.stdout
        );
        assert!(
            !out.stderr.contains(secret),
            "secret on stderr: {}",
            out.stderr
        );
    }
    // Config, state, sessions and the project (bin/ holds the test's own token helpers).
    for sub in ["home", "project"] {
        let files = files_containing(&dir.join(sub), secret);
        assert!(files.is_empty(), "secret in files: {files:?}");
    }
}

fn provider_report(doctor: &Value, id: &str) -> Value {
    doctor["models"]["providers"]
        .as_array()
        .unwrap_or_else(|| panic!("no providers: {doctor}"))
        .iter()
        .find(|p| p["id"] == id)
        .unwrap_or_else(|| panic!("no provider {id}: {doctor}"))
        .clone()
}

fn check(doctor: &Value, name: &str) -> Value {
    doctor["checks"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["name"] == name)
        .unwrap_or_else(|| panic!("no check {name}: {doctor}"))
        .clone()
}

/// A program that prints `body` (and nothing on stderr), made executable.
fn script(path: &Path, body: &str) {
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, format!("#!/bin/sh\n{body}\n")).unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
}

async fn mock(key: &str, key_header: Option<&str>, models: &[&str]) -> MockProvider {
    spawn_provider(ProviderState {
        key: Some(key.to_owned()),
        key_header: key_header.map(str::to_owned),
        models: models.iter().map(|m| (*m).to_owned()).collect(),
        reply: "pong".into(),
        ..ProviderState::default()
    })
    .await
}

#[tokio::test]
async fn connect_header_auth_with_query_org_and_project_then_run_and_doctor() {
    let guard = spawn_proxy_guard().await;
    let amp = spawn(MockAmp::new(), None).await;
    let endpoint = mock(KEY, Some("api-key"), &["m-1"]).await;
    let dir = workdir("auth-2277-header", None);
    let env = Env::new(&dir, &guard)
        .var("GW_API_KEY", KEY)
        .var("AMP_MCP_URL", amp.url.clone());
    let out = env
        .run(
            &[
                "connect",
                "gw",
                "--base-url",
                &endpoint.url,
                "--auth",
                "header",
                "--query",
                "api-version=2024-10-21",
                "--organization",
                "org-2277",
                "--project",
                "proj_2277",
                "--model",
                "m-1",
                "--use",
                "--role-model",
                "m-1",
                "--json",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let v = out.json();
    assert_eq!(v["check"], "ok", "{v}");
    assert_eq!(v["key"], "env GW_API_KEY", "{v}");
    let text = config(&dir);
    for want in [
        "auth = \"header\"",
        "query = { \"api-version\" = \"2024-10-21\" }",
        "organization = \"org-2277\"",
        "project = \"proj_2277\"",
        "key_env = \"GW_API_KEY\"",
    ] {
        assert!(text.contains(want), "missing {want}:\n{text}");
    }

    // A headless run sends the key in api-key, the query and the org/project headers.
    let run = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(run.code, 0, "{}\n{}", run.stdout, run.stderr);
    let (uri, headers) = endpoint
        .requests()
        .into_iter()
        .rfind(|(u, _)| u.contains("/chat/completions"))
        .expect("a chat request");
    assert_eq!(uri, "/v1/chat/completions?api-version=2024-10-21");
    assert_eq!(headers["api-key"], KEY);
    assert_eq!(headers["openai-organization"], "org-2277");
    assert_eq!(headers["openai-project"], "proj_2277");
    assert!(!headers.contains_key("authorization"), "{headers:?}");

    // doctor: the auth type, never the key.
    let doctor = env.run(&["doctor", "--json"], None).await;
    let d = doctor.json();
    let p = provider_report(&d, "gw");
    assert_eq!(p["auth"], "header");
    assert_eq!(p["auth_detail"], "key in the api-key header");
    assert_eq!(check(&d, "models:gw")["status"], "ok", "{d}");
    let human = env.run(&["doctor"], None).await;
    assert!(
        human.stdout.contains("auth: key in the api-key header"),
        "{}",
        human.stdout
    );
    no_secret(KEY, &[&out, &run, &doctor, &human], &dir);
    assert!(guard.seen().is_empty(), "{:?}", guard.seen());
}

#[tokio::test]
async fn a_token_command_provider_connects_runs_and_doctor_shows_the_expiry() {
    let guard = spawn_proxy_guard().await;
    let endpoint = mock(COMMAND_TOKEN, None, &["m-1"]).await;
    let dir = workdir("auth-2277-command", None);
    let helper = dir.join("bin/token-helper");
    script(
        &helper,
        &format!("echo '{{\"token\":\"{COMMAND_TOKEN}\",\"expires_in\":3600}}'"),
    );
    let amp = spawn(MockAmp::new(), None).await;
    let env = Env::new(&dir, &guard).var("AMP_MCP_URL", amp.url.clone());
    let command = format!("/bin/sh {}", helper.display());
    let out = env
        .run(
            &[
                "connect",
                "sso",
                "--base-url",
                &endpoint.url,
                "--auth",
                "command",
                "--auth-command",
                &command,
                "--model",
                "m-1",
                "--use",
                "--role-model",
                "m-1",
                "--json",
            ],
            None,
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert_eq!(out.json()["check"], "ok", "{}", out.stdout);
    let text = config(&dir);
    assert!(text.contains("auth = \"command\""), "{text}");
    assert!(
        text.contains(&format!(
            "auth_command = [\"/bin/sh\", \"{}\"]",
            helper.display()
        )),
        "{text}"
    );
    assert!(!text.contains("key_env"), "a command needs no key: {text}");

    let run = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(run.code, 0, "{}\n{}", run.stdout, run.stderr);
    assert!(
        endpoint
            .requests()
            .iter()
            .any(|(u, h)| u.contains("/chat/completions")
                && h.get("authorization").map(String::as_str)
                    == Some(format!("Bearer {COMMAND_TOKEN}").as_str()))
    );

    let doctor = env.run(&["doctor", "--json"], None).await;
    let d = doctor.json();
    let p = provider_report(&d, "sso");
    assert_eq!(p["auth"], "command");
    assert!(
        p["token_expires"]
            .as_str()
            .is_some_and(|t| t.ends_with("UTC")),
        "{p}"
    );
    assert_eq!(check(&d, "key:sso")["status"], "ok");
    assert!(
        check(&d, "key:sso")["detail"]
            .as_str()
            .unwrap()
            .contains("token from `/bin/sh`"),
        "{d}"
    );
    let human = env.run(&["doctor"], None).await;
    assert!(
        human.stdout.contains("token valid until"),
        "{}",
        human.stdout
    );

    // `connect sso` again: nothing to store, the token is checked.
    let again = env.run(&["connect", "sso"], None).await;
    assert_eq!(again.code, 0, "{}\n{}", again.stdout, again.stderr);
    assert!(
        again.stderr.contains("there is no key to store"),
        "{}",
        again.stderr
    );
    no_secret(COMMAND_TOKEN, &[&out, &run, &doctor, &human, &again], &dir);
}

#[tokio::test]
async fn the_first_run_sets_up_entra_through_the_azure_cli() {
    let guard = spawn_proxy_guard().await;
    let amp = spawn(MockAmp::new(), None).await;
    let endpoint = mock(AZ_TOKEN, None, &["gpt-4o"]).await;
    let dir = workdir("auth-2277-entra-az", None);
    // A fake Azure CLI: `az account get-access-token --scope … --output json`.
    script(
        &dir.join("bin/az"),
        &format!(
            "case \"$*\" in *get-access-token*cognitiveservices*) ;; *) echo \"unexpected: $*\" >&2; exit 2 ;; esac\n\
             echo \"{{\\\"accessToken\\\": \\\"{AZ_TOKEN}\\\", \\\"expires_on\\\": $(( $(date +%s) + 3600 )), \\\"tokenType\\\": \\\"Bearer\\\"}}\""
        ),
    );
    let path = format!(
        "{}:{}",
        dir.join("bin").display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let env = Env::new(&dir, &guard)
        .var("PATH", path)
        .var("AMP_HARNESS_PROMPTS", "stdin")
        .var("AMP_MCP_URL", amp.url.clone());
    let host = endpoint
        .url
        .trim_start_matches("http://")
        .trim_end_matches("/v1")
        .to_owned();
    // No project; provider 4 (own endpoint); name; id; the address; confirm the /v1 URL;
    // authentication 6 (Entra); no tenant (the Azure CLI); no extra header. No key is asked; the
    // one model runs every role.
    let answers = format!("\n4\nCorp AOAI\naoai\n{host}\n\n6\n\n\n");
    let out = env
        .run(&["--snapshot", "100x20", "--no-session"], Some(&answers))
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    for want in [
        "How does your endpoint authenticate?",
        "6) Microsoft Entra ID",
        "Every role runs on aoai/gpt-4o.",
    ] {
        assert!(
            out.stderr.contains(want),
            "missing {want:?}:\n{}",
            out.stderr
        );
    }
    assert!(
        !out.stderr.contains("Corp AOAI API key") && !out.stderr.contains("client secret"),
        "no key is asked: {}",
        out.stderr
    );
    let text = config(&dir);
    assert!(text.contains("auth = \"azure-entra\""), "{text}");
    assert!(!text.contains("tenant_id"), "{text}");

    let run = env.run(&["run", "-p", "hello", "--json"], None).await;
    assert_eq!(run.code, 0, "{}\n{}", run.stdout, run.stderr);
    let doctor = env.run(&["doctor"], None).await;
    assert!(
        doctor
            .stdout
            .contains("auth: Microsoft Entra ID, token from `az`, token valid until"),
        "{}",
        doctor.stdout
    );
    no_secret(AZ_TOKEN, &[&out, &run, &doctor], &dir);
    assert!(guard.seen().is_empty(), "{:?}", guard.seen());
}
