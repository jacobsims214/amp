//! #2165: the real macOS keychain, end to end, with the built binary. Opt-in: set
//! `AMP_TEST_KEYCHAIN=1` (local runs on a Mac; Linux CI has no login keychain and skips it).
//!
//! Every child runs with `AMP_HARNESS_KEYCHAIN_PROMPTS=never`, so a keychain call that would need a
//! dialog fails at once instead of showing one: a passing read proves the item needed no approval.
//! Items live under a per-test service (`amp-harness-test-kc-<pid>-<case>`), never the user's
//! `amp-harness` entries, and are deleted afterwards with `security` (deleting never prompts).
//! `AMP_HARNESS_KEYCHAIN_TRACE` records every real keychain call, so the tests count them per process.

#![cfg(target_os = "macos")]

mod common;

use std::{collections::BTreeMap, path::PathBuf, process::Stdio};

use common::{
    MockAmp, Output,
    provider::{ProviderState, files_containing, spawn_provider},
    spawn, workdir,
};
use serde_json::{Value, json};

const SECRET: &str = "sk-or-v1-keychain-test-0123456789abcdef";

fn enabled() -> bool {
    if std::env::var("AMP_TEST_KEYCHAIN").is_ok_and(|v| v == "1") {
        return true;
    }
    eprintln!("skipped: set AMP_TEST_KEYCHAIN=1 to run the real-keychain tests (macOS)");
    false
}

/// Deletes this test's items however the test ends.
struct Cleanup {
    service: String,
    accounts: Vec<&'static str>,
}

impl Drop for Cleanup {
    fn drop(&mut self) {
        for account in &self.accounts {
            let _ = std::process::Command::new("security")
                .args([
                    "delete-generic-password",
                    "-s",
                    &self.service,
                    "-a",
                    account,
                ])
                .output();
        }
    }
}

struct Case {
    dir: PathBuf,
    service: String,
    trace: PathBuf,
    url: String,
}

impl Case {
    fn new(name: &str, url: &str, config: &str) -> Self {
        let dir = workdir(&format!("keychain-{name}"), Some(2));
        let config_dir = dir.join("home/.config/amp-harness");
        std::fs::create_dir_all(&config_dir).unwrap();
        std::fs::write(config_dir.join("config.toml"), config).unwrap();
        Self {
            service: format!("amp-harness-test-kc-{}-{name}", std::process::id()),
            trace: dir.join("keychain-trace.tsv"),
            url: url.to_owned(),
            dir,
        }
    }

    /// The binary with the real HOME (the login keychain lives there), config under the test dir,
    /// the keychain store under this case's service, no key in the environment, and no dialogs.
    async fn run(&self, args: &[&str], env: &[(&str, &str)]) -> Output {
        self.run_with(args, env, None).await
    }

    /// [`Case::run`] with `stdin` piped in.
    async fn run_with(&self, args: &[&str], env: &[(&str, &str)], stdin: Option<&str>) -> Output {
        use tokio::io::AsyncWriteExt;
        let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(args)
            .current_dir(self.dir.join("project"))
            .env("XDG_CONFIG_HOME", self.dir.join("home/.config"))
            .env("XDG_STATE_HOME", self.dir.join("home/.local/state"))
            .env("AMP_CREDENTIAL_STORE", "keychain")
            .env("AMP_HARNESS_KEYCHAIN_SERVICE", &self.service)
            .env("AMP_HARNESS_KEYCHAIN_PROMPTS", "never")
            .env("AMP_HARNESS_KEYCHAIN_TRACE", &self.trace)
            .env("AMP_HARNESS_CEILING_DIR", &self.dir)
            .env("AMP_MCP_URL", &self.url)
            .env("AMP_WEB_URL", "http://ui.invalid")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_PROMPTS")
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
        let script = self.dir.join("script.json");
        if script.exists() {
            cmd.env("AMP_HARNESS_FAUX_SCRIPT", script);
        } else {
            cmd.env_remove("AMP_HARNESS_FAUX_SCRIPT");
        }
        for (k, v) in env {
            cmd.env(k, v);
        }
        let mut child = cmd.spawn().expect("spawn amp-harness");
        if let Some(text) = stdin {
            let mut pipe = child.stdin.take().unwrap();
            pipe.write_all(text.as_bytes()).await.unwrap();
            drop(pipe);
        }
        let out =
            tokio::time::timeout(std::time::Duration::from_secs(60), child.wait_with_output())
                .await
                .expect("amp-harness finished within 60 s (a keychain dialog would block it)")
                .expect("run amp-harness");
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr: String::from_utf8(out.stderr).unwrap(),
        }
    }

    /// Trace lines `(op, account, outcome)` per pid, in call order.
    fn calls_by_pid(&self) -> BTreeMap<String, Vec<(String, String, String)>> {
        let text = std::fs::read_to_string(&self.trace).unwrap_or_default();
        let mut by_pid: BTreeMap<String, Vec<_>> = BTreeMap::new();
        for line in text.lines() {
            let f: Vec<&str> = line.split('\t').collect();
            assert_eq!(f.len(), 5, "trace line: {line}");
            assert_eq!(
                f[1], self.service,
                "a call outside the test service: {line}"
            );
            by_pid.entry(f[4].to_owned()).or_default().push((
                f[0].to_owned(),
                f[2].to_owned(),
                f[3].to_owned(),
            ));
        }
        by_pid
    }

    fn clear_trace(&self) {
        let _ = std::fs::remove_file(&self.trace);
    }

    /// The one process in the trace; fails if each item was not read at most once.
    fn single_process_reads(&self) -> Vec<(String, String, String)> {
        let by_pid = self.calls_by_pid();
        assert_eq!(
            by_pid.len(),
            1,
            "expected one process in the trace: {by_pid:?}"
        );
        let calls = by_pid.into_values().next().unwrap();
        let mut reads: BTreeMap<&str, usize> = BTreeMap::new();
        for (op, account, _) in &calls {
            if op == "read" {
                *reads.entry(account).or_default() += 1;
            }
        }
        for (account, n) in &reads {
            assert_eq!(
                *n, 1,
                "{account} was read {n} times in one process: {calls:?}"
            );
        }
        calls
    }
}

const FAUX_MANAGER: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]
[roles.manager]
model = "faux/faux-1"
"#;

/// A key stored by the harness is read back by the same build in a NEW process with no dialog
/// (prompts are off, so a needed one would fail the read), and a whole `run -p`, which builds the
/// providers more than once and talks to AMP many times, reads each keychain item at most once.
#[tokio::test]
async fn a_harness_made_item_is_read_without_a_prompt_once_per_process() {
    if !enabled() {
        return;
    }
    let amp = spawn(MockAmp::new(), None).await;
    let case = Case::new("roundtrip", &amp.url, FAUX_MANAGER);
    let _cleanup = Cleanup {
        service: case.service.clone(),
        accounts: vec!["provider:openrouter"],
    };
    std::fs::write(
        case.dir.join("script.json"),
        json!([{"text": "hello from faux"}]).to_string(),
    )
    .unwrap();

    // Process 1 stores the key (init --key-env), as the harness: it is the item's creator.
    let out = case
        .run(
            &[
                "init",
                "--yes",
                "--no-project",
                "--key-env",
                "OPENROUTER_API_KEY",
                "--json",
            ],
            &[("OPENROUTER_API_KEY", SECRET)],
        )
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    let calls = case.single_process_reads();
    assert!(
        calls
            .iter()
            .any(|(op, a, o)| op == "write" && a == "provider:openrouter" && o == "ok"),
        "{calls:?}"
    );
    // The key it stored is never read back.
    assert!(
        !calls
            .iter()
            .any(|(op, a, o)| op == "read" && a == "provider:openrouter" && o == "found"),
        "{calls:?}"
    );

    // Process 2: run -p with no key in the environment. The openrouter key comes from the
    // keychain without a dialog, and nothing is read twice.
    case.clear_trace();
    let out = case.run(&["run", "-p", "hi", "--json"], &[]).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    let calls = case.single_process_reads();
    assert!(
        calls
            .iter()
            .any(|(op, a, o)| op == "read" && a == "provider:openrouter" && o == "found"),
        "the new process should have read the key: {calls:?}"
    );
    assert!(
        !calls.iter().any(|(_, _, o)| o.starts_with("error")),
        "{calls:?}"
    );
    eprintln!("run -p keychain calls: {calls:?}");

    // Process 3: config show reports the keychain as the key's source.
    case.clear_trace();
    let out = case.run(&["config", "show", "--json"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let v: Value = serde_json::from_str(&out.stdout).unwrap();
    assert!(v["providers"].to_string().contains("keychain"), "{v}");
    case.single_process_reads();
}

/// An item this binary is not trusted for (made by Apple's `security` tool, which is exactly the
/// user's #2165 report) fails fast in a run with no terminal: one read, one clear message, no
/// dialog, no loop.
#[tokio::test]
async fn an_untrusted_item_fails_fast_with_one_message() {
    if !enabled() {
        return;
    }
    let amp = spawn(MockAmp::new(), None).await;
    // The shipped roles: the manager needs openrouter.
    let case = Case::new("untrusted", &amp.url, "");
    let _cleanup = Cleanup {
        service: case.service.clone(),
        accounts: vec!["provider:openrouter"],
    };
    let added = std::process::Command::new("security")
        .args([
            "add-generic-password",
            "-s",
            &case.service,
            "-a",
            "provider:openrouter",
            "-w",
            SECRET,
        ])
        .output()
        .unwrap();
    assert!(
        added.status.success(),
        "security add-generic-password failed"
    );

    let started = std::time::Instant::now();
    let out = case.run(&["run", "-p", "hi"], &[]).await;
    let took = started.elapsed();
    assert_ne!(out.code, 0, "stdout: {}", out.stdout);
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    assert!(
        out.stderr.contains("amp-harness connect openrouter")
            && out.stderr.contains("provider:openrouter"),
        "stderr: {}",
        out.stderr
    );
    assert!(took.as_secs() < 20, "took {took:?}");
    let calls = case.single_process_reads();
    assert!(
        calls.iter().any(|(op, a, o)| op == "read"
            && a == "provider:openrouter"
            && o.starts_with("error:")),
        "{calls:?}"
    );
    eprintln!("untrusted item: {calls:?}\nstderr: {}", out.stderr);

    // Storing the key through the harness replaces the item with one this build created, and a
    // new process then reads it without a dialog.
    case.clear_trace();
    let out = case
        .run(
            &[
                "init",
                "--yes",
                "--no-project",
                "--key-env",
                "OPENROUTER_API_KEY",
            ],
            &[("OPENROUTER_API_KEY", SECRET)],
        )
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let calls = case.single_process_reads();
    assert!(
        calls
            .iter()
            .any(|(op, a, o)| op == "write" && a == "provider:openrouter" && o == "ok"),
        "{calls:?}"
    );
    eprintln!("re-store over an untrusted item: {calls:?}");
    case.clear_trace();
    let out = case.run(&["config", "show", "--json"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let calls = case.single_process_reads();
    assert!(
        calls
            .iter()
            .any(|(op, a, o)| op == "read" && a == "provider:openrouter" && o == "found"),
        "after re-storing through the harness: {calls:?}"
    );
}

// --- #2163: connect / disconnect / the chat's /connect ---------------------------------------------

const MOCK_SECRET: &str = "sk-mock-2163-keychain-0123456789abcdef";

/// `connect` checks the key with the provider, stores it through the harness, and a new process
/// reads it without a dialog; `disconnect` removes it. The chat's `/connect` stores a key and the
/// running chat uses it at once: `/model` switches to the provider that had no key at start, and
/// the next prompt reaches the provider with the new key.
#[tokio::test]
async fn connect_stores_a_checked_key_and_the_chat_uses_it_without_a_restart() {
    if !enabled() {
        return;
    }
    let amp = spawn(MockAmp::new(), None).await;
    let provider = spawn_provider(ProviderState {
        key: Some(MOCK_SECRET.into()),
        models: vec!["m-1".into()],
        reply: "hello from the mock provider".into(),
        ..ProviderState::default()
    })
    .await;
    let config = format!(
        "{FAUX_MANAGER}\n[providers.mock]\nname = \"Mock AI\"\napi = \"openai-completions\"\n\
         base_url = \"{}\"\nkey_env = \"MOCK_KEY\"\n[providers.mock.models.\"m-1\"]\n",
        provider.url
    );
    let case = Case::new("connect", &amp.url, &config);
    let _cleanup = Cleanup {
        service: case.service.clone(),
        accounts: vec!["provider:mock"],
    };

    // A wrong key: rejected, nothing written.
    let out = case
        .run_with(&["connect", "mock", "--key-stdin"], &[], Some("sk-wrong\n"))
        .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(
        !case
            .calls_by_pid()
            .values()
            .flatten()
            .any(|(op, _, _)| op == "write"),
        "{:?}",
        case.calls_by_pid()
    );

    // The right one: checked, then stored by the harness.
    case.clear_trace();
    let out = case
        .run_with(
            &["connect", "mock", "--key-stdin"],
            &[],
            Some(&format!("{MOCK_SECRET}\n")),
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(
        out.stderr.contains("Mock AI accepted the key"),
        "{}",
        out.stderr
    );
    assert!(
        out.stdout.contains("Connected Mock AI (mock)"),
        "{}",
        out.stdout
    );
    assert_eq!(
        provider.seen().last().unwrap().1.as_deref(),
        Some(format!("Bearer {MOCK_SECRET}").as_str())
    );

    // A new process reads it, once, with no dialog.
    case.clear_trace();
    let out = case.run(&["connect", "--list", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let mock = out
        .json()
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == "mock")
        .cloned()
        .unwrap();
    assert_eq!(mock["key"], "keychain", "{mock}");
    let calls = case.single_process_reads();
    assert!(
        calls
            .iter()
            .any(|(op, a, o)| op == "read" && a == "provider:mock" && o == "found"),
        "{calls:?}"
    );

    // disconnect removes it.
    let out = case.run(&["disconnect", "mock"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(
        out.stdout.contains("Removed the Mock AI key"),
        "{}",
        out.stdout
    );
    let out = case.run(&["connect", "--list", "--json"], &[]).await;
    let mock = out
        .json()
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == "mock")
        .cloned()
        .unwrap();
    assert_eq!(mock["key"], "missing", "{mock}");

    // The chat starts without the key, /connect stores it, and the same chat uses it.
    std::fs::write(
        case.dir.join("script.json"),
        json!([{"text": "unused"}]).to_string(),
    )
    .unwrap();
    let before = provider.seen().len();
    let paste = format!("key:paste:{MOCK_SECRET}");
    let out = case
        .run(
            &[
                "--snapshot",
                "160x40",
                "--input",
                "/model mock/m-1",
                "--input",
                "/connect mock",
                "--input",
                &paste,
                "--input",
                "key:enter",
                "--input",
                "/model mock/m-1",
                "--input",
                "say hello",
            ],
            &[],
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let screen = &out.stdout;
    assert!(
        screen.contains("unknown model `mock/m-1`"),
        "no key at start: {screen}"
    );
    assert!(screen.contains("Mock AI accepted the key"), "{screen}");
    assert!(screen.contains("model: mock/m-1"), "{screen}");
    assert!(screen.contains("hello from the mock provider"), "{screen}");
    let chat = &provider.seen()[before..];
    assert!(
        chat.iter()
            .any(|(path, auth, _)| path == "/v1/chat/completions"
                && auth.as_deref() == Some(format!("Bearer {MOCK_SECRET}").as_str())),
        "{chat:?}"
    );
    // Never in the output, the transcript, the session file or the config.
    assert!(!out.stdout.contains(MOCK_SECRET) && !out.stderr.contains(MOCK_SECRET));
    let files = files_containing(&case.dir, MOCK_SECRET);
    assert!(files.is_empty(), "the key is in {files:?}");
}

/// A custom provider with a secret header, entered in the (scripted) terminal: the header value
/// and the key go to the keychain, the table to config.toml, and a headless run sends both.
#[tokio::test]
async fn a_custom_provider_keeps_its_secret_header_in_the_keychain() {
    if !enabled() {
        return;
    }
    let amp = spawn(MockAmp::new(), None).await;
    let header_secret = "hdr-2163-0123456789abcdefSECRET";
    let provider = spawn_provider(ProviderState {
        key: Some(MOCK_SECRET.into()),
        header: Some(("X-Api-Key".into(), header_secret.into())),
        models: vec!["m-1".into(), "m-2".into()],
        reply: "hello through the gateway".into(),
        ..ProviderState::default()
    })
    .await;
    let case = Case::new("custom", &amp.url, FAUX_MANAGER);
    let _cleanup = Cleanup {
        service: case.service.clone(),
        accounts: vec!["provider:gw", "provider:gw:header:x-api-key"],
    };
    // Display name, API (1), a header (secret by name: read hidden), no more headers, the key,
    // models (Enter: all), roles (y), the model every role uses (Enter: the first).
    let answers = format!("Gateway\n1\nX-Api-Key\n{header_secret}\n\n{MOCK_SECRET}\n\ny\n\n");
    let out = case
        .run_with(
            &["connect", "gw", "--base-url", &provider.url],
            &[("AMP_HARNESS_PROMPTS", "stdin")],
            Some(&answers),
        )
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(
        out.stderr.contains("Gateway accepted the key"),
        "{}",
        out.stderr
    );
    assert!(out.stdout.contains("inherit gw/m-1"), "{}", out.stdout);
    let text =
        std::fs::read_to_string(case.dir.join("home/.config/amp-harness/config.toml")).unwrap();
    assert!(text.contains("secret_headers = [\"X-Api-Key\"]"), "{text}");
    for s in [MOCK_SECRET, header_secret] {
        assert!(!out.stdout.contains(s) && !out.stderr.contains(s));
        assert!(files_containing(&case.dir, s).is_empty(), "{s} in a file");
    }

    // A headless worker run sends the key and the header from the keychain.
    std::fs::write(
        case.dir.join("home/.config/amp-harness/config.toml"),
        text.replace("[roles.manager]\nmodel = \"faux/faux-1\"\n", ""),
    )
    .unwrap();
    let out = case
        .run(
            &["run", "-p", "hi", "--role", "worker-docs", "--no-session"],
            &[],
        )
        .await;
    assert_eq!(
        out.code,
        0,
        "{}\n{}\n{:?}",
        out.stdout,
        out.stderr,
        provider.seen()
    );
    assert!(
        out.stdout.contains("hello through the gateway"),
        "{}",
        out.stdout
    );
    assert!(
        provider
            .seen()
            .iter()
            .any(|(path, auth, h)| path == "/v1/chat/completions"
                && auth.as_deref() == Some(format!("Bearer {MOCK_SECRET}").as_str())
                && h.as_deref() == Some(header_secret)),
        "{:?}",
        provider.seen()
    );

    // disconnect --keep-config removes both keychain items (roles still use gw).
    let out = case
        .run(&["disconnect", "gw", "--keep-config", "--json"], &[])
        .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let v = out.json();
    assert_eq!(v["key_removed"], true);
    assert_eq!(v["headers_removed"], json!(["X-Api-Key"]));
}
