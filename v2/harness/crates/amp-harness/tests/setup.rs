//! M4-1: `amp-harness init` and the chat's first run, against the mock AMP with a temp HOME
//! (under cargo's target dir, D13). Scripted answers come from stdin (`AMP_HARNESS_PROMPTS=stdin`).
//! Provider keys must end up only in the OS keychain: never in a file, never in the output.

mod common;

use std::{
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};

use common::{MockAmp, Output, spawn, workdir};
use serde_json::{Value, json};
use tokio::io::AsyncWriteExt;

/// A key-shaped value that must never show up in output or files.
const SECRET: &str = "sk-test-2113-never-print-me";

struct Run<'a> {
    dir: &'a Path,
    args: &'a [&'a str],
    env: Vec<(&'a str, String)>,
    unset: Vec<&'a str>,
    stdin: Option<&'a str>,
    cwd: Option<PathBuf>,
    real_home: bool,
}

impl<'a> Run<'a> {
    fn new(dir: &'a Path, args: &'a [&'a str]) -> Self {
        Self {
            dir,
            args,
            env: Vec::new(),
            unset: Vec::new(),
            stdin: None,
            cwd: None,
            real_home: false,
        }
    }

    fn env(mut self, k: &'a str, v: impl Into<String>) -> Self {
        self.env.push((k, v.into()));
        self
    }

    async fn go(self) -> Output {
        let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(self.args)
            .current_dir(self.cwd.unwrap_or_else(|| self.dir.join("project")))
            .env("XDG_CONFIG_HOME", self.dir.join("home/.config"))
            .env("AMP_CREDENTIAL_STORE", "file")
            .env("AMP_HARNESS_CEILING_DIR", self.dir)
            // The compose UI may be running on this machine; never let the web probe find it.
            .env("AMP_WEB_URL", "http://ui.invalid")
            .env_remove("AMP_MCP_URL")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_CA_FILE")
            .env_remove("AMP_MCP_PROTOCOL")
            .env_remove("AMP_HARNESS_PROFILE")
            .env_remove("AMP_HARNESS_PROMPTS")
            .env_remove("AMP_HARNESS_KEYCHAIN_SERVICE")
            .env_remove("ANTHROPIC_API_KEY")
            .env_remove("OPENROUTER_API_KEY")
            .env_remove("RUST_LOG")
            .stdin(if self.stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        if !self.real_home {
            cmd.env("HOME", self.dir.join("home"));
        }
        for k in self.unset {
            cmd.env_remove(k);
        }
        for (k, v) in self.env {
            cmd.env(k, v);
        }
        let mut child = cmd.spawn().expect("spawn amp-harness");
        if let Some(text) = self.stdin {
            let mut stdin = child.stdin.take().unwrap();
            stdin.write_all(text.as_bytes()).await.unwrap();
            drop(stdin);
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
}

fn config_text(dir: &Path) -> String {
    std::fs::read_to_string(dir.join("home/.config/amp-harness/config.toml")).unwrap_or_default()
}

fn marker(dir: &Path) -> Value {
    let text = std::fs::read_to_string(dir.join("project/.amp.json")).expect(".amp.json written");
    serde_json::from_str(&text).unwrap()
}

/// No file under `dir` contains `needle`.
fn assert_not_in_files(dir: &Path, needle: &str) {
    fn walk(p: &Path, needle: &str) {
        for entry in std::fs::read_dir(p).unwrap().flatten() {
            let path = entry.path();
            if path.is_dir() {
                walk(&path, needle);
            } else if let Ok(bytes) = std::fs::read(&path) {
                assert!(
                    !String::from_utf8_lossy(&bytes).contains(needle),
                    "secret found in {}",
                    path.display()
                );
            }
        }
    }
    walk(dir, needle);
}

#[tokio::test]
async fn init_with_flags_saves_the_server_and_links_a_project() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-flags", None);
    let out = Run::new(
        &dir,
        &[
            "init",
            "--yes",
            "--url",
            &amp.url,
            "--project",
            "2",
            "--json",
        ],
    )
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let summary = out.json();
    assert_eq!(summary["server"], amp.url.as_str());
    assert_eq!(summary["user"], "local-dev@example.com");
    assert_eq!(summary["project"]["id"], 2);
    assert_eq!(summary["project"]["created"], false);
    assert_eq!(summary["profile"], "opencode");
    assert_eq!(
        summary["providers"]["openrouter"]
            .as_str()
            .unwrap()
            .split(' ')
            .next(),
        Some("missing")
    );
    assert!(
        summary["warnings"][0]
            .as_str()
            .unwrap()
            .contains("no model provider key")
    );

    let config = config_text(&dir);
    assert!(
        config.contains(&format!("url = \"{}\"", amp.url)),
        "{config}"
    );
    let m = marker(&dir);
    assert_eq!(m["project_id"], 2);
    assert_eq!(m["project_name"], "AMP v2");
    // D4: the real AMP origin, not a stale value.
    assert_eq!(m["amp_api"], amp.url.trim_end_matches("/mcp"));

    // The next command finds the server and the project without flags.
    // (doctor exits 1 here: no provider key is a failed check since #2114.)
    let out = Run::new(&dir, &["doctor", "--json"]).go().await;
    let doctor = out.json();
    let text = doctor.to_string();
    assert!(text.contains(&amp.url), "{text}");
    assert!(text.contains("AMP v2"), "{text}");
}

#[tokio::test]
async fn init_creates_a_project() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-create", None);
    let out = Run::new(
        &dir,
        &[
            "init",
            "--yes",
            "--url",
            &amp.url,
            "--create",
            "Setup Demo",
            "--json",
        ],
    )
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert_eq!(out.json()["project"]["created"], true);
    let calls = amp.mock.calls_to("amp_create_project");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0]["name"], "Setup Demo");
    assert_eq!(calls[0]["code"], "SD");
    assert_eq!(marker(&dir)["project_id"], 77);
}

#[tokio::test]
async fn init_asks_for_what_is_missing() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-interactive", None);
    // Server, then project 1 from the picker. The keychain is off (file store), so no key prompt.
    let answers = format!("{}\n1\n", amp.url.trim_end_matches("/mcp"));
    let mut run = Run::new(&dir, &["init", "--json"]);
    run.stdin = Some(&answers);
    let out = run.env("AMP_HARNESS_PROMPTS", "stdin").go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stderr.contains("AMP server ["), "{}", out.stderr);
    assert!(out.stderr.contains("1) #2 AMP v2 (AV)"), "{}", out.stderr);
    assert!(
        !out.stderr.contains("#9 Old"),
        "archived projects are not offered"
    );
    assert!(
        out.stderr.contains("Keys are kept only in the OS keychain"),
        "{}",
        out.stderr
    );
    assert!(out.stderr.contains("OPENROUTER_API_KEY"), "{}", out.stderr);
    // The answer had no /mcp: setup added it and saved the URL after connecting.
    assert!(config_text(&dir).contains(&format!("url = \"{}\"", amp.url)));
    assert_eq!(marker(&dir)["project_id"], 2);
    assert_eq!(out.json()["project"]["id"], 2);
}

#[tokio::test]
async fn init_without_a_keychain_refuses_to_store_a_key_anywhere() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-no-keychain", None);
    let out = Run::new(
        &dir,
        &[
            "init",
            "--yes",
            "--url",
            &amp.url,
            "--no-project",
            "--key-env",
            "OPENROUTER_API_KEY",
        ],
    )
    .env("OPENROUTER_API_KEY", SECRET)
    .go()
    .await;
    assert_eq!(
        out.code, 1,
        "stdout: {}\nstderr: {}",
        out.stdout, out.stderr
    );
    assert!(
        out.stderr.contains("only kept in the OS keychain"),
        "{}",
        out.stderr
    );
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    assert_not_in_files(&dir, SECRET);
}

#[tokio::test]
async fn an_anthropic_key_alone_selects_the_anthropic_profile() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-anthropic", None);
    let out = Run::new(
        &dir,
        &["init", "--yes", "--url", &amp.url, "--no-project", "--json"],
    )
    .env("ANTHROPIC_API_KEY", SECRET)
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let summary = out.json();
    assert_eq!(summary["profile"], "anthropic");
    assert_eq!(summary["providers"]["anthropic"], "env ANTHROPIC_API_KEY");
    assert!(summary["warnings"].as_array().unwrap().is_empty());
    assert!(config_text(&dir).contains("[models]\nprofile = \"anthropic\""));
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    assert_not_in_files(&dir, SECRET);
}

#[tokio::test]
async fn init_never_opens_a_login_with_yes() {
    let amp = spawn(MockAmp::new(), Some("right-token")).await;
    let dir = workdir("setup-login", None);
    let out = Run::new(&dir, &["init", "--yes", "--url", &amp.url, "--no-project"])
        .go()
        .await;
    assert_eq!(out.code, 1);
    assert!(out.stderr.contains("needs a login"), "{}", out.stderr);
    assert!(out.stderr.contains("amp-harness login"), "{}", out.stderr);
    // Nothing is saved for a server that could not be used.
    assert!(!config_text(&dir).contains("url ="));

    // --token-env: the token is used, never written.
    let out = Run::new(
        &dir,
        &[
            "init",
            "--yes",
            "--url",
            &amp.url,
            "--token-env",
            "MY_AMP",
            "--no-project",
            "--json",
        ],
    )
    .env("MY_AMP", "right-token")
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.json()["auth"].as_str().unwrap().contains("MY_AMP"));
    assert_not_in_files(&dir, "right-token");
}

#[tokio::test]
async fn the_chat_first_run_links_a_new_project_and_suggests_seeding_it() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-chat", None);
    let mut run = Run::new(&dir, &["--snapshot", "120x30", "--no-session"]);
    run.stdin = Some("n\nNew Thing\n\n");
    let out = run
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("AMP_MCP_URL", amp.url.clone())
        .env("OPENROUTER_API_KEY", SECRET)
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stderr.contains("Created #77 New Thing (NT)"),
        "{}",
        out.stderr
    );
    // The URL came from the environment: the first run doesn't ask for or save a server.
    assert!(!out.stderr.contains("AMP server ["), "{}", out.stderr);
    assert!(
        out.stdout.contains("project #77 New Thing"),
        "{}",
        out.stdout
    );
    assert!(
        out.stdout.contains("Set up this new project"),
        "{}",
        out.stdout
    );
    assert_eq!(marker(&dir)["project_id"], 77);
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));

    // Second start: the marker is there, nothing is asked.
    let out = Run::new(&dir, &["--snapshot", "120x30", "--no-session"])
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("AMP_MCP_URL", amp.url.clone())
        .env("OPENROUTER_API_KEY", SECRET)
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stderr.trim().is_empty(), "{}", out.stderr);
    assert!(!out.stdout.contains("Set up this new project"));
}

#[tokio::test]
async fn a_taken_project_code_is_asked_again() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-code-taken", None);
    // n, a name, a code an archived project already has, then a free one.
    let args = ["init", "--url", &amp.url, "--json"];
    let mut run = Run::new(&dir, &args);
    run.stdin = Some("n\nNew Thing\nold\nNT\n");
    let out = run.env("AMP_HARNESS_PROMPTS", "stdin").go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stderr.contains("project code \"OLD\" already exists"),
        "{}",
        out.stderr
    );
    let codes: Vec<Value> = amp
        .mock
        .calls_to("amp_create_project")
        .iter()
        .map(|c| c["code"].clone())
        .collect();
    assert_eq!(codes, vec![json!("OLD"), json!("NT")]);
    assert_eq!(out.json()["project"]["created"], true);
    assert_eq!(marker(&dir)["project_id"], 77);
}

#[tokio::test]
async fn the_chat_without_a_key_says_how_to_store_one() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-chat-nokey", Some(2));
    // Scripted answers but no key and no keychain: the first run stops with the way out. The
    // marker was made for another server (#2206), so the picker comes first: Enter keeps it.
    let mut run = Run::new(&dir, &["--snapshot", "100x20", "--no-session"]);
    run.stdin = Some("\n");
    let out = run
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("AMP_MCP_URL", amp.url.clone())
        .go()
        .await;
    assert_eq!(out.code, 1, "stdout: {}", out.stdout);
    assert!(out.stderr.contains("Keeping #2 from "), "{}", out.stderr);
    // AMP_CREDENTIAL_STORE=file: connect can't store a key, so the way out is the variable.
    assert!(
        out.stderr
            .contains("turned off (AMP_CREDENTIAL_STORE=file)"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr
            .contains("`export OPENROUTER_API_KEY=… && amp-harness`"),
        "{}",
        out.stderr
    );

    // Without setup (no terminal, no script): the session error names connect too.
    let out = Run::new(&dir, &["--snapshot", "100x20", "--no-session"])
        .env("AMP_MCP_URL", amp.url.clone())
        .go()
        .await;
    assert_eq!(out.code, 1);
    assert!(out.stderr.contains("amp-harness connect"), "{}", out.stderr);
}

/// #2214 (QA-1 #12): no usable keychain (a HOME without one: macOS finds no default keychain,
/// Linux CI has no Secret Service) and no key in the environment. The first run still offers the
/// picker and links the project, then ends with one message naming the next command; the platform
/// error shows only with --verbose. A per-test keychain service and no dialogs, so the user's
/// keychain is never touched.
#[tokio::test]
async fn without_a_keychain_the_first_run_links_the_project_then_says_the_next_step() {
    let amp = spawn(MockAmp::new(), None).await;
    for verbose in [false, true] {
        let dir = workdir(&format!("setup-no-keychain-{verbose}"), None);
        let args: &[&str] = if verbose {
            &["--snapshot", "100x20", "--no-session", "--verbose"]
        } else {
            &["--snapshot", "100x20", "--no-session"]
        };
        let service = format!("amp-harness-test-2214-{}", std::process::id());
        let mut run = Run::new(&dir, args);
        run.stdin = Some("n\nNew Thing\n\n");
        run.unset.push("AMP_CREDENTIAL_STORE");
        let out = run
            .env("AMP_HARNESS_PROMPTS", "stdin")
            .env("AMP_MCP_URL", amp.url.clone())
            .env("AMP_HARNESS_KEYCHAIN_SERVICE", service)
            .env("AMP_HARNESS_KEYCHAIN_PROMPTS", "never")
            .go()
            .await;
        assert_eq!(
            out.code, 1,
            "stdout: {}\nstderr: {}",
            out.stdout, out.stderr
        );
        // The project step ran first and linked the folder.
        assert!(
            out.stderr.contains("Created #77 New Thing (NT)"),
            "{}",
            out.stderr
        );
        assert_eq!(marker(&dir)["project_id"], 77);
        // Then exactly one message about the key, with the next command.
        assert_eq!(
            out.stderr.matches("model provider key").count(),
            1,
            "{}",
            out.stderr
        );
        assert!(
            out.stderr
                .contains("`export OPENROUTER_API_KEY=… && amp-harness`"),
            "{}",
            out.stderr
        );
        // The raw keychain error (which names the probe item) only with --verbose.
        assert_eq!(out.stderr.contains("  cause: "), verbose, "{}", out.stderr);
        if !verbose {
            assert!(!out.stderr.contains("amp-harness-probe"), "{}", out.stderr);
        }
    }
}

/// Deletes the test keychain service's provider entry (macOS `security`, which never prompts for
/// a delete by service/account).
#[cfg(target_os = "macos")]
struct KeychainCleanup(String);

#[cfg(target_os = "macos")]
impl Drop for KeychainCleanup {
    fn drop(&mut self) {
        let _ = std::process::Command::new("security")
            .args([
                "delete-generic-password",
                "-s",
                &self.0,
                "-a",
                "provider:openrouter",
            ])
            .output();
    }
}

/// The real keychain round trip, under a per-test service so the user's entries are untouched.
/// macOS only (the login keychain lives under the real HOME); skipped with AMP_TEST_SKIP_KEYCHAIN.
#[cfg(target_os = "macos")]
#[tokio::test]
async fn a_key_goes_to_the_keychain_and_nowhere_else() {
    if std::env::var_os("AMP_TEST_SKIP_KEYCHAIN").is_some() {
        eprintln!("skipped: AMP_TEST_SKIP_KEYCHAIN");
        return;
    }
    let service = format!("amp-harness-test-{}", std::process::id());
    if amp_mcp::keychain_available(&service).is_err() {
        eprintln!("skipped: no keychain");
        return;
    }
    let _cleanup = KeychainCleanup(service.clone());
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-keychain", None);
    let run = |args: &'static [&'static str]| {
        let mut r = Run::new(&dir, args);
        r.real_home = true;
        r.env("AMP_CREDENTIAL_STORE", "keychain")
            .env("AMP_HARNESS_KEYCHAIN_SERVICE", service.clone())
            .env("AMP_MCP_URL", amp.url.clone())
    };
    let out = run(&[
        "init",
        "--yes",
        "--no-project",
        "--key-env",
        "OPENROUTER_API_KEY",
        "--json",
    ])
    .env("OPENROUTER_API_KEY", SECRET)
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stderr
            .contains("Stored the OpenRouter key from OPENROUTER_API_KEY")
    );
    assert!(!out.stdout.contains(SECRET) && !out.stderr.contains(SECRET));
    assert_not_in_files(&dir, SECRET);

    // Without the variable, the key comes from the keychain (same build: no access prompt).
    let out = run(&["config", "show", "--json"]).go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let text = out.stdout.clone();
    assert!(!text.contains(SECRET));
    let v: Value = serde_json::from_str(&text).unwrap();
    let providers = v["providers"].to_string();
    assert!(providers.contains("keychain"), "{providers}");
}

/// #2206: a parent folder's `.amp.json` made for another server (prod) is not used silently on
/// this one: the first run says so and offers the picker; the pick is written in the working
/// folder, so the parent's marker is shadowed, not rewritten. The next start asks nothing.
#[tokio::test]
async fn a_parent_marker_made_for_another_server_offers_the_picker() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-foreign-parent", Some(2));
    let parent = dir.join("project/.amp.json");
    let before = std::fs::read_to_string(&parent).unwrap();
    let sub = dir.join("project/tools/sub");
    std::fs::create_dir_all(&sub).unwrap();

    let mut run = Run::new(&dir, &["--snapshot", "400x30", "--no-session"]);
    run.cwd = Some(sub.clone());
    run.stdin = Some("1\n");
    let out = run
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("AMP_MCP_URL", amp.url.clone())
        .env("OPENROUTER_API_KEY", SECRET)
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    for want in [
        format!(
            "Warning: .amp.json at {} was made for https://stale.invalid; this server's #2 is \
             \"AMP v2\".",
            parent.display()
        ),
        "Pick this server's project for this folder, or create a new one:".to_owned(),
        "1) #2 AMP v2 (AV)".to_owned(),
        "Enter to keep #2 from .amp.json".to_owned(),
        "Using #2 AMP v2 (AV) and linked this folder".to_owned(),
    ] {
        assert!(
            out.stderr.contains(&want),
            "missing {want:?} in\n{}",
            out.stderr
        );
    }
    let written: Value =
        serde_json::from_str(&std::fs::read_to_string(sub.join(".amp.json")).unwrap()).unwrap();
    assert_eq!(written["project_id"], 2);
    assert_eq!(written["amp_api"], amp.url.trim_end_matches("/mcp"));
    assert_eq!(std::fs::read_to_string(&parent).unwrap(), before);
    // The banner has no warning: the new marker is this server's.
    assert!(!out.stdout.contains("was made for"), "{}", out.stdout);

    // Second start: the folder's own marker agrees with the server; nothing is asked.
    let mut run = Run::new(&dir, &["--snapshot", "400x30", "--no-session"]);
    run.cwd = Some(sub);
    let out = run
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("AMP_MCP_URL", amp.url.clone())
        .env("OPENROUTER_API_KEY", SECRET)
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stderr.trim().is_empty(), "{}", out.stderr);
}

/// #2206 in `init`: interactive, a marker made for another server goes to the picker (not "Keep
/// it?") and is replaced in place; with --yes it is kept and the summary warns.
#[tokio::test]
async fn init_offers_the_picker_for_a_marker_made_for_another_server() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-foreign-init", Some(2));
    let want = format!(
        ".amp.json at {} was made for https://stale.invalid; this server's #2 is \"AMP v2\"",
        dir.join("project/.amp.json").display()
    );

    let out = Run::new(&dir, &["init", "--yes", "--url", &amp.url, "--json"])
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let v = out.json();
    assert_eq!(v["project"]["id"], 2);
    assert!(
        v["warnings"]
            .as_array()
            .unwrap()
            .iter()
            .any(|w| w == want.as_str()),
        "{v}"
    );
    assert_eq!(marker(&dir)["amp_api"], "https://stale.invalid");

    let args = ["init", "--url", &amp.url, "--json"];
    let mut run = Run::new(&dir, &args);
    run.stdin = Some("1\n");
    let out = run.env("AMP_HARNESS_PROMPTS", "stdin").go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stderr.contains(&want), "{}", out.stderr);
    assert!(!out.stderr.contains("Keep it?"), "{}", out.stderr);
    assert!(
        out.stderr
            .contains("Pick this server's project for this folder"),
        "{}",
        out.stderr
    );
    let m = marker(&dir);
    assert_eq!(m["project_id"], 2);
    assert_eq!(m["amp_api"], amp.url.trim_end_matches("/mcp"));
}

/// #2212: the first run said "Saved the server" but `amp-harness server` then said "No saved
/// servers". The first run and init save the server as a named entry, as `server set` does.
#[tokio::test]
async fn the_first_run_saves_the_server_by_name() {
    let one = spawn(MockAmp::new(), None).await;
    let two = spawn(MockAmp::new(), None).await;
    let dir = workdir("setup-saved-server", None);
    // Server (without /mcp), then project 1 from the picker.
    let answers = format!("{}\n1\n", one.url.trim_end_matches("/mcp"));
    let mut run = Run::new(&dir, &["--snapshot", "120x30", "--no-session"]);
    run.stdin = Some(&answers);
    let out = run
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env("OPENROUTER_API_KEY", SECRET)
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stderr
            .contains(&format!("Saved the server {} as `local`", one.url)),
        "{}",
        out.stderr
    );
    let config = config_text(&dir);
    assert!(config.contains("[servers.local]"), "{config}");
    assert!(config.contains("name = \"local\""), "{config}");

    // `amp-harness server` lists it, saved and active.
    let out = Run::new(&dir, &["server"]).go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!out.stdout.contains("No saved servers"), "{}", out.stdout);
    assert!(out.stdout.contains("AMP server: local"), "{}", out.stdout);
    assert!(
        out.stdout.contains(&format!("* local  {}", one.url)),
        "{}",
        out.stdout
    );
    let shown = Run::new(&dir, &["server", "--json"]).go().await.json();
    assert_eq!(shown["name"], "local");
    assert_eq!(shown["saved"][0]["name"], "local");
    assert_eq!(shown["saved"][0]["url"], one.url.as_str());

    // init --url another server: saved next to the first, which `server use` brings back.
    let out = Run::new(
        &dir,
        &["init", "--yes", "--url", &two.url, "--no-project", "--json"],
    )
    .go()
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let shown = Run::new(&dir, &["server", "--json"]).go().await.json();
    let names: Vec<&str> = shown["saved"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s["name"].as_str().unwrap())
        .collect();
    assert_eq!(names.len(), 2, "{shown}");
    assert!(names.contains(&"local"), "{shown}");
    assert_ne!(shown["name"], "local", "{shown}");
    assert_eq!(shown["url"]["value"], two.url.as_str());

    let out = Run::new(&dir, &["server", "use", "local"]).go().await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let shown = Run::new(&dir, &["server", "--json"]).go().await.json();
    assert_eq!(shown["name"], "local");
    assert_eq!(shown["url"]["value"], one.url.as_str());

    // Running init again for the active server changes nothing and says nothing about saving.
    let out = Run::new(&dir, &["init", "--yes", "--url", &one.url, "--no-project"])
        .go()
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!out.stderr.contains("Saved the server"), "{}", out.stderr);
}
