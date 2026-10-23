//! Trust for repo-level MCP servers (#2241, H8) end to end: an MCP server from the repo's
//! `.amp/harness/config.toml` never starts without consent (headless `run -p` skips it with a
//! warning, a worker's worktree instance is never spawned), `--trust-repo-mcp` /
//! `AMP_HARNESS_TRUST_REPO_MCP=1` start them without recording anything, `mcp trust` records the
//! server's current config in `<state>/trust.toml` (outside the repo) and a change to it asks
//! again, `mcp untrust` forgets, and the chat asks before its hub starts.
//!
//! The "servers" are `/bin/sh -c 'touch <marker>; exec sleep 30'`: the marker file exists iff the
//! harness ever spawned the command. The http server is a bare TCP listener: no connection means
//! the harness never contacted it. Scratch dirs and HOMEs are under cargo's target dir (D13).

mod common;

use std::{
    io::Write as _,
    path::{Path, PathBuf},
    process::Stdio,
    sync::Arc,
    time::Duration,
};

use amp_core::{
    mcp_config::McpConfig,
    mcp_hub::{HubOptions, McpHub, ServerStatus},
    mcp_trust::TrustStore,
};
use amp_mcp::ConfigPaths;
use common::{MockAmp, Output, spawn, workdir};
use serde_json::{Value, json};

const FAUX: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

/// A stdio server table whose command only touches `marker` (then idles: no MCP handshake).
fn marker_table(name: &str, marker: &Path, extra: &str) -> String {
    let script = format!("touch '{}'; exec sleep 30", marker.display());
    format!(
        "[mcp.{name}]\ncommand = \"/bin/sh\"\nargs = [\"-c\", {script:?}]\nstartup_timeout_secs = 1\n{extra}\n"
    )
}

struct Repo {
    dir: PathBuf,
}

impl Repo {
    /// A work dir (project 2): the faux models + `user_mcp` in the user config.toml, `repo_mcp` in
    /// `project/.amp/harness/config.toml`.
    fn new(name: &str, user_mcp: &str, repo_mcp: &str) -> Self {
        let dir = workdir(&format!("mcp-trust-{name}"), Some(2));
        let config = dir.join("home/.config/amp-harness");
        std::fs::create_dir_all(&config).unwrap();
        std::fs::write(config.join("config.toml"), format!("{FAUX}\n{user_mcp}")).unwrap();
        std::fs::write(
            dir.join("script.json"),
            json!([{"text": "ok"}, {"text": "ok"}, {"text": "ok"}]).to_string(),
        )
        .unwrap();
        let repo = Self { dir };
        repo.write_repo(repo_mcp);
        repo
    }

    fn write_repo(&self, repo_mcp: &str) {
        let harness = self.dir.join("project/.amp/harness");
        std::fs::create_dir_all(&harness).unwrap();
        std::fs::write(harness.join("config.toml"), repo_mcp).unwrap();
    }

    fn marker(&self, name: &str) -> PathBuf {
        self.dir.join(format!("ran-{name}"))
    }

    fn clear_markers(&self) {
        for e in std::fs::read_dir(&self.dir).unwrap() {
            let p = e.unwrap().path();
            if p.file_name()
                .is_some_and(|n| n.to_string_lossy().starts_with("ran-"))
            {
                std::fs::remove_file(p).unwrap();
            }
        }
    }

    fn ran(&self, name: &str) -> bool {
        self.marker(name).exists()
    }

    fn state(&self) -> PathBuf {
        self.dir.join("home/.local/state")
    }

    fn trust_file(&self) -> PathBuf {
        self.state().join("amp-harness/trust.toml")
    }

    fn env(&self) -> Vec<(String, String)> {
        vec![
            (
                "AMP_HARNESS_FAUX_SCRIPT".into(),
                self.dir.join("script.json").display().to_string(),
            ),
            ("XDG_STATE_HOME".into(), self.state().display().to_string()),
        ]
    }

    async fn run(&self, url: &str, args: &[&str], extra: &[(&str, &str)]) -> Output {
        let env = self.env();
        let mut all: Vec<(&str, &str)> =
            env.iter().map(|(k, v)| (k.as_str(), v.as_str())).collect();
        all.extend_from_slice(extra);
        common::run(&self.dir, url, args, &all).await
    }

    /// The chat (`--snapshot`) with scripted answers on stdin.
    async fn chat(&self, url: &str, answers: &str) -> Output {
        let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(["--no-setup", "--snapshot", "160x40"])
            .current_dir(self.dir.join("project"))
            .env("HOME", self.dir.join("home"))
            .env("XDG_CONFIG_HOME", self.dir.join("home/.config"))
            .env("AMP_CREDENTIAL_STORE", "file")
            .env("AMP_MCP_URL", url)
            .env("AMP_HARNESS_CEILING_DIR", &self.dir)
            .env("AMP_HARNESS_PROMPTS", "stdin")
            .env_remove("AMP_TOKEN")
            .env_remove("AMP_HARNESS_TRUST_REPO_MCP")
            .env_remove("RUST_LOG")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for (k, v) in self.env() {
            cmd.env(k, v);
        }
        let mut child = cmd.spawn().unwrap();
        {
            use tokio::io::AsyncWriteExt;
            let mut stdin = child.stdin.take().unwrap();
            stdin.write_all(answers.as_bytes()).await.unwrap();
            // Dropped: end of input after the answers.
        }
        let out = tokio::time::timeout(Duration::from_secs(60), child.wait_with_output())
            .await
            .expect("the chat finished within 60 s")
            .unwrap();
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr: String::from_utf8(out.stderr).unwrap(),
        }
    }
}

fn session(out: &Output) -> Value {
    let first = out.stdout.lines().next().unwrap_or_default();
    serde_json::from_str(first).unwrap_or_else(|e| panic!("not JSON ({e}): {}", out.stdout))
}

fn state_of(session: &Value, name: &str) -> String {
    session["mcp_servers"]
        .as_array()
        .unwrap()
        .iter()
        .find(|s| s["name"] == name)
        .unwrap_or_else(|| panic!("no {name} in {session}"))["status"]["state"]
        .as_str()
        .unwrap()
        .to_owned()
}

/// A TCP listener standing in for an http MCP server; `contacted` says whether anyone connected.
struct Listener(std::net::TcpListener);

impl Listener {
    fn new() -> Self {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.set_nonblocking(true).unwrap();
        Self(l)
    }
    fn url(&self) -> String {
        format!("http://{}/mcp", self.0.local_addr().unwrap())
    }
    fn contacted(&self) -> bool {
        self.0.accept().is_ok()
    }
}

#[tokio::test]
async fn untrusted_repo_servers_never_start_headless() {
    let amp = spawn(MockAmp::new(), None).await;
    let web = Listener::new();
    let repo = Repo::new("headless", "", "");
    repo.write_repo(&format!(
        "{}\n[mcp.web]\nurl = {:?}\nstartup_timeout_secs = 1\n",
        marker_table("fs", &repo.marker("fs"), ""),
        web.url()
    ));
    // A user-level server is the person's own: it starts without asking.
    std::fs::write(
        repo.dir.join("home/.config/amp-harness/config.toml"),
        format!("{FAUX}\n{}", marker_table("mine", &repo.marker("mine"), "")),
    )
    .unwrap();

    let out = repo
        .run(&amp.url, &["run", "-p", "hi", "--json"], &[])
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!repo.ran("fs"), "the untrusted stdio server was spawned");
    assert!(!web.contacted(), "the untrusted http server was contacted");
    assert!(repo.ran("mine"), "the user's own server did not start");
    let s = session(&out);
    assert_eq!(state_of(&s, "fs"), "untrusted");
    assert_eq!(state_of(&s, "web"), "untrusted");
    for name in ["fs", "web"] {
        assert!(
            out.stderr.contains(&format!(
                "amp-harness: MCP server `{name}`: not started, not trusted"
            )),
            "{}",
            out.stderr
        );
        assert!(
            out.stderr
                .contains(&format!("`amp-harness mcp trust {name}`")),
            "{}",
            out.stderr
        );
    }
    assert!(out.stderr.contains("--trust-repo-mcp"), "{}", out.stderr);
    // Headless never records anything.
    assert!(!repo.trust_file().exists());
}

#[tokio::test]
async fn the_override_starts_repo_servers_without_recording() {
    let amp = spawn(MockAmp::new(), None).await;
    let repo = Repo::new("override", "", "");
    repo.write_repo(&marker_table("fs", &repo.marker("fs"), ""));

    // The flag is global: before the subcommand or after it.
    for args in [
        &["--trust-repo-mcp", "run", "-p", "hi"][..],
        &["run", "-p", "hi", "--trust-repo-mcp"][..],
    ] {
        repo.clear_markers();
        let out = repo.run(&amp.url, args, &[]).await;
        assert_eq!(out.code, 0, "{args:?} stderr: {}", out.stderr);
        assert!(repo.ran("fs"), "{args:?} did not start the server");
        assert!(!out.stderr.contains("not trusted"), "{}", out.stderr);
    }
    repo.clear_markers();
    let out = repo
        .run(
            &amp.url,
            &["run", "-p", "hi"],
            &[("AMP_HARNESS_TRUST_REPO_MCP", "1")],
        )
        .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(repo.ran("fs"));
    assert!(!repo.trust_file().exists(), "the override was recorded");
}

#[tokio::test]
async fn mcp_trust_records_outside_the_repo_and_a_change_asks_again() {
    let amp = spawn(MockAmp::new(), None).await;
    let repo = Repo::new(
        "cli",
        "[mcp.mine]\ncommand = \"mine\"\nenabled = false\n",
        "",
    );
    let two = |fs_extra: &str| {
        format!(
            "{}{}",
            marker_table("fs", &repo.marker("fs"), fs_extra),
            marker_table("db", &repo.marker("db"), "")
        )
    };
    repo.write_repo(&two(""));

    // The list.
    let out = repo.run(&amp.url, &["mcp", "trust", "--json"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let rows = out.json();
    let row = |rows: &Value, n: &str| {
        rows.as_array()
            .unwrap()
            .iter()
            .find(|r| r["name"] == n)
            .unwrap()
            .clone()
    };
    assert_eq!(row(&rows, "fs")["trust"], "untrusted");
    assert_eq!(row(&rows, "fs")["layer"], "repo");
    assert_eq!(row(&rows, "mine")["trust"], "user");

    // A user server, an unknown one: refused.
    let out = repo.run(&amp.url, &["mcp", "trust", "mine"], &[]).await;
    assert_eq!(out.code, 2, "stderr: {}", out.stderr);
    assert!(out.stderr.contains("always trusted"), "{}", out.stderr);
    let out = repo.run(&amp.url, &["mcp", "trust", "nope"], &[]).await;
    assert_eq!(out.code, 2, "stderr: {}", out.stderr);
    assert!(!repo.trust_file().exists());

    // Trust one: recorded in the state dir, nothing in the repo; it starts, the other doesn't.
    let out = repo.run(&amp.url, &["mcp", "trust", "fs"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(
        out.stdout.contains("trusted fs: runs `/bin/sh -c"),
        "{}",
        out.stdout
    );
    let text = std::fs::read_to_string(repo.trust_file()).unwrap();
    let project = std::fs::canonicalize(repo.dir.join("project")).unwrap();
    assert!(
        text.contains(&format!("{:?}", project.display().to_string())),
        "{text}"
    );
    assert!(text.contains("[repos."), "{text}");
    let in_repo: Vec<_> = std::fs::read_dir(repo.dir.join("project/.amp/harness"))
        .unwrap()
        .map(|e| e.unwrap().file_name())
        .collect();
    assert_eq!(in_repo, ["config.toml"]);
    let out = repo.run(&amp.url, &["run", "-p", "hi"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(repo.ran("fs"));
    assert!(!repo.ran("db"));
    assert!(
        out.stderr.contains("MCP server `db`: not started"),
        "{}",
        out.stderr
    );
    // (It fails its handshake: the stand-in is no MCP server. It was not refused.)
    assert!(
        !out.stderr.contains("MCP server `fs`: not started"),
        "{}",
        out.stderr
    );

    // The repo changes fs's config: not trusted any more, and the warning says why.
    repo.clear_markers();
    repo.write_repo(&two("env = { EXTRA = \"1\" }"));
    let out = repo.run(&amp.url, &["mcp", "trust", "--json"], &[]).await;
    assert_eq!(row(&out.json(), "fs")["trust"], "changed");
    let out = repo.run(&amp.url, &["run", "-p", "hi"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!repo.ran("fs"), "a changed server started without asking");
    assert!(
        out.stderr.contains("changed since you trusted it"),
        "{}",
        out.stderr
    );

    // --all trusts both as they are now; untrust forgets one, then all.
    let out = repo.run(&amp.url, &["mcp", "trust", "--all"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    repo.clear_markers();
    let out = repo.run(&amp.url, &["run", "-p", "hi"], &[]).await;
    assert!(repo.ran("fs") && repo.ran("db"), "stderr: {}", out.stderr);
    let out = repo.run(&amp.url, &["mcp", "untrust", "fs"], &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert_eq!(out.stdout.trim(), "untrusted fs");
    let out = repo.run(&amp.url, &["mcp", "trust", "--json"], &[]).await;
    assert_eq!(row(&out.json(), "fs")["trust"], "untrusted");
    assert_eq!(row(&out.json(), "db")["trust"], "trusted");
    let out = repo.run(&amp.url, &["mcp", "untrust", "--all"], &[]).await;
    assert_eq!(out.stdout.trim(), "untrusted db");
    let out = repo.run(&amp.url, &["mcp", "trust", "--json"], &[]).await;
    assert_eq!(row(&out.json(), "db")["trust"], "untrusted");
}

#[tokio::test]
async fn the_chat_asks_before_repo_servers_start() {
    let amp = spawn(MockAmp::new(), None).await;
    let repo = Repo::new("chat", "", "");
    // Eager: the chat waits for their first attempt, so a trusted one has run by the end.
    let eager = "startup = \"eager\"";
    repo.write_repo(&format!(
        "{}{}{}",
        marker_table("a", &repo.marker("a"), eager),
        marker_table("b", &repo.marker("b"), eager),
        marker_table("c", &repo.marker("c"), eager)
    ));

    // a: no, b: yes, c: Enter (no).
    let out = repo.chat(&amp.url, "n\ny\n\n").await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    assert!(
        out.stderr
            .contains("defines MCP servers you have not trusted"),
        "{}",
        out.stderr
    );
    assert!(
        out.stderr.contains("`a` runs `/bin/sh -c"),
        "{}",
        out.stderr
    );
    assert!(!repo.ran("a") && repo.ran("b") && !repo.ran("c"));
    assert!(
        out.stderr.contains("Trusted `b` (recorded in"),
        "{}",
        out.stderr
    );
    // The transcript says which ones were left off, and how to trust them.
    let screen = out.stdout.split_whitespace().collect::<Vec<_>>().join(" ");
    assert!(
        screen.contains("MCP server `a` not started"),
        "{}",
        out.stdout
    );
    let store = TrustStore::load(&repo.trust_file()).unwrap();
    let project = std::fs::canonicalize(repo.dir.join("project")).unwrap();
    let names: Vec<String> = store
        .servers(&project)
        .into_iter()
        .map(|(n, _)| n)
        .collect();
    assert_eq!(names, ["b"]);

    // Next time only a and c are asked; `a` (all) trusts both.
    repo.clear_markers();
    let out = repo.chat(&amp.url, "a\n").await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!out.stderr.contains("`b` runs"), "{}", out.stderr);
    assert!(repo.ran("a") && repo.ran("b") && repo.ran("c"));

    // Everything trusted: no question at all (stdin is empty).
    repo.clear_markers();
    let out = repo.chat(&amp.url, "").await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(!out.stderr.contains("not trusted"), "{}", out.stderr);
    assert!(repo.ran("a"));
}

/// Workers never ask: an untrusted `scope = "worktree"` repo server gets no instance; once
/// trusted (the chat's answer), the next worker gets one.
#[tokio::test]
async fn worker_worktrees_skip_untrusted_repo_servers() {
    let repo = Repo::new("worktree", "", "");
    repo.write_repo(&marker_table(
        "wt",
        &repo.marker("wt"),
        "scope = \"worktree\"\ncwd = \"${worktree}\"",
    ));
    let paths = ConfigPaths {
        user_file: Some(repo.dir.join("home/.config/amp-harness/config.toml")),
        user_dir: Some(repo.dir.join("home/.config/amp-harness")),
        repo_dir: Some(repo.dir.join("project/.amp/harness")),
        ..ConfigPaths::default()
    };
    let config = McpConfig::load(&paths);
    assert!(config.errors().next().is_none(), "{:?}", config.problems);
    let state = repo.state().join("amp-harness");
    let mut opts = HubOptions::new(paths, state.clone(), repo.dir.join("project"));
    opts.keychain = None;
    opts.trust_all = false;
    let hub = Arc::new(McpHub::new(config, opts));
    hub.start();
    let health = hub.health();
    assert!(
        matches!(health[0].status, ServerStatus::Untrusted { .. }),
        "{health:?}"
    );
    let worktree = repo.dir.join("project/.worktrees/7");
    std::fs::create_dir_all(&worktree).unwrap();
    assert!(hub.start_worktree("7", &worktree).await.is_none());
    assert!(!repo.ran("wt"));

    hub.trust("wt").unwrap();
    let set = hub.start_worktree("8", &worktree).await;
    assert!(set.is_some());
    assert!(repo.ran("wt"));
    hub.shutdown().await;
}

#[test]
fn an_unreadable_trust_store_trusts_nothing() {
    let repo = Repo::new("broken-store", "", "");
    repo.write_repo(&marker_table("fs", &repo.marker("fs"), ""));
    let state = repo.state().join("amp-harness");
    std::fs::create_dir_all(&state).unwrap();
    let mut f = std::fs::File::create(state.join("trust.toml")).unwrap();
    f.write_all(b"repos = \"nonsense\"\n").unwrap();
    let paths = ConfigPaths {
        user_file: Some(repo.dir.join("home/.config/amp-harness/config.toml")),
        repo_dir: Some(repo.dir.join("project/.amp/harness")),
        ..ConfigPaths::default()
    };
    let mut opts = HubOptions::new(paths.clone(), state, repo.dir.clone());
    opts.trust_all = false;
    let hub = McpHub::new(McpConfig::load(&paths), opts);
    assert_eq!(hub.untrusted().len(), 1);
    // A restart (`/mcp restart`) does not get around it either.
    let err = hub.restart("fs").unwrap_err();
    assert!(err.contains("is not trusted"), "{err}");
    assert!(!repo.ran("fs"));
}

/// #2242: `/reload` takes trust given in another terminal (`mcp trust`), and names a repo server
/// added since that still needs it, without starting it.
#[tokio::test]
async fn a_reload_takes_trust_given_elsewhere() {
    let repo = Repo::new("reload", "", "");
    repo.write_repo(&marker_table("fs", &repo.marker("fs"), ""));
    let paths = ConfigPaths {
        user_file: Some(repo.dir.join("home/.config/amp-harness/config.toml")),
        user_dir: Some(repo.dir.join("home/.config/amp-harness")),
        repo_dir: Some(repo.dir.join("project/.amp/harness")),
        ..ConfigPaths::default()
    };
    let mut opts = HubOptions::new(
        paths.clone(),
        repo.state().join("amp-harness"),
        repo.dir.join("project"),
    );
    opts.keychain = None;
    opts.trust_all = false;
    let hub = McpHub::new(McpConfig::load(&paths), opts);
    hub.start();
    assert_eq!(hub.untrusted().len(), 1);

    let out = repo
        .run("http://127.0.0.1:9/mcp", &["mcp", "trust", "fs"], &[])
        .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    // Another repo server appears at the same time: named, not started.
    repo.write_repo(&format!(
        "{}{}",
        marker_table("fs", &repo.marker("fs"), ""),
        marker_table("new", &repo.marker("new"), "")
    ));
    let report = hub.reload(McpConfig::load(&paths)).await;
    assert_eq!(report.added, vec!["new"], "{report:?}");
    assert_eq!(report.untrusted, vec!["new"], "{report:?}");
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    while !repo.ran("fs") && std::time::Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(
        repo.ran("fs"),
        "trusted elsewhere: it starts after the reload"
    );
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!repo.ran("new"));
    let names: Vec<String> = hub.untrusted().into_iter().map(|u| u.name).collect();
    assert_eq!(names, vec!["new"]);
    hub.shutdown().await;
}
