//! `amp-harness mcp import` (#2244, H10) end to end with the built binary: Claude Code (.mcp.json,
//! ~/.claude.json user and local scope), opencode, VS Code (inputs), Cursor and Claude Desktop
//! fixtures; `--dry-run`; clashes, `--rename`, `--only`, `--force`; repo scope trust; secrets moved
//! to the keychain and never written to a file or printed; and (opt-in, macOS: `AMP_TEST_KEYCHAIN=1`)
//! the real keychain under a test-only service.
//!
//! Every HOME, config and source file is a fixture under cargo's target dir (D13). The real
//! ~/.claude.json, ~/.cursor, ~/Library/Application Support/… are never read: HOME and
//! XDG_CONFIG_HOME point at the fixture, and the keychain case (which needs the real HOME for the
//! login keychain) names its source with `--from FILE`, so nothing else is looked at.

mod common;

use std::{
    io::Write as _,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};

use common::{Output, workdir};
use serde_json::{Value, json};

/// Secret-shaped values planted in fixtures; they must never reach a file or an output.
const PLANTED: &str = "sk-import-planted-0123456789abcdef";
const PLANTED_ENV: &str = "ghp_importplanted0123456789";

struct T {
    dir: PathBuf,
    keychain: Option<String>,
}

impl T {
    fn new(name: &str) -> Self {
        let dir = workdir(&format!("mcp-import-{name}"), Some(2));
        std::fs::create_dir_all(dir.join("home/.config/amp-harness")).unwrap();
        Self {
            dir,
            keychain: None,
        }
    }

    fn project(&self) -> PathBuf {
        std::fs::canonicalize(self.dir.join("project")).unwrap()
    }

    fn user_file(&self) -> PathBuf {
        self.dir.join("home/.config/amp-harness/config.toml")
    }

    fn repo_file(&self) -> PathBuf {
        self.dir.join("project/.amp/harness/config.toml")
    }

    fn write(&self, rel: &str, text: &str) -> PathBuf {
        let p = self.dir.join(rel);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(&p, text).unwrap();
        p
    }

    fn run(&self, args: &[&str]) -> Output {
        self.run_with(args, &[], None)
    }

    fn run_with(&self, args: &[&str], env: &[(&str, &str)], stdin: Option<&str>) -> Output {
        let dir = &self.dir;
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_amp-harness"));
        cmd.args(args)
            .current_dir(dir.join("project"))
            .env("XDG_CONFIG_HOME", dir.join("home/.config"))
            .env("XDG_STATE_HOME", dir.join("home/.local/state"))
            .env("AMP_MCP_URL", "http://127.0.0.1:9/mcp")
            .env("AMP_HARNESS_CEILING_DIR", dir)
            .env("AMP_HARNESS_KEYCHAIN_PROMPTS", "never")
            .env_remove("AMP_HARNESS_PROMPTS")
            .env_remove("AMP_HARNESS_TRUST_REPO_MCP")
            .env_remove("AMP_HARNESS_CONFIG")
            .env_remove("AMP_HARNESS_CONFIG_DIR")
            .env_remove("CLAUDE_CONFIG_DIR")
            .env_remove("OPENCODE_CONFIG")
            .env_remove("AMP_TOKEN")
            .env_remove("RUST_LOG")
            .stdin(if stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        match &self.keychain {
            Some(service) => {
                cmd.env_remove("AMP_CREDENTIAL_STORE")
                    .env("AMP_HARNESS_KEYCHAIN_SERVICE", service);
            }
            None => {
                cmd.env("HOME", dir.join("home"))
                    .env("AMP_CREDENTIAL_STORE", "file")
                    .env_remove("AMP_HARNESS_KEYCHAIN_SERVICE");
            }
        }
        for (k, v) in env {
            cmd.env(k, v);
        }
        let mut child = cmd.spawn().expect("spawn amp-harness");
        if let Some(input) = stdin {
            let mut pipe = child.stdin.take().unwrap();
            pipe.write_all(input.as_bytes()).unwrap();
        }
        let deadline = Instant::now() + Duration::from_secs(60);
        while child.try_wait().unwrap().is_none() {
            if Instant::now() > deadline {
                let _ = child.kill();
                panic!("amp-harness {args:?} did not finish within 60 s");
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        let out = child.wait_with_output().unwrap();
        Output {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8(out.stdout).unwrap(),
            stderr: String::from_utf8(out.stderr).unwrap(),
        }
    }

    /// No file under the test dir holds `secret`.
    fn assert_no_file_has(&self, secret: &str) {
        fn walk(dir: &Path, secret: &str) {
            for e in std::fs::read_dir(dir).unwrap().flatten() {
                let p = e.path();
                if p.is_dir() {
                    walk(&p, secret);
                } else if let Ok(bytes) = std::fs::read(&p) {
                    let fixture = p.file_name().is_some_and(|n| {
                        let n = n.to_string_lossy();
                        n.ends_with(".json") || n.ends_with(".jsonc")
                    });
                    assert!(
                        fixture || !String::from_utf8_lossy(&bytes).contains(secret),
                        "{} holds a secret",
                        p.display()
                    );
                }
            }
        }
        walk(&self.dir, secret);
    }
}

fn ok(out: &Output) -> &Output {
    assert_eq!(
        out.code, 0,
        "stdout:\n{}\nstderr:\n{}",
        out.stdout, out.stderr
    );
    out
}

fn no_secret(out: &Output, secret: &str) {
    assert!(
        !out.stdout.contains(secret) && !out.stderr.contains(secret),
        "a secret was printed"
    );
}

fn row<'a>(v: &'a Value, source_name: &str) -> &'a Value {
    v["servers"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["source_name"] == source_name)
        .unwrap_or_else(|| panic!("no row for {source_name} in {v}"))
}

fn rows_named<'a>(v: &'a Value, source_name: &str) -> Vec<&'a Value> {
    v["servers"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|r| r["source_name"] == source_name)
        .collect()
}

fn toml_of(r: &Value) -> toml::Table {
    let text = r["toml"].as_str().unwrap_or_else(|| panic!("no toml: {r}"));
    let doc: toml::Table = toml::from_str(text).unwrap();
    doc["mcp"][r["name"].as_str().unwrap()]
        .as_table()
        .unwrap()
        .clone()
}

fn read(p: &Path) -> String {
    std::fs::read_to_string(p).unwrap_or_default()
}

/// Claude Code's three places: local (~/.claude.json projects[<project>]), project (.mcp.json) and
/// user (~/.claude.json root), with local > project > user for one name.
fn claude_fixture(t: &T) {
    let project = t.project().display().to_string();
    t.write(
        "home/.claude.json",
        &json!({
            "numStartups": 3,
            "mcpServers": {
                "user-srv": {"command": "user-cmd", "args": ["--x"]},
                "dup": {"command": "from-user"}
            },
            "projects": {
                project: {
                    "mcpServers": {
                        "Local_Srv": {"type": "stdio", "command": "local-cmd", "env": {"MODE": "${MODE:-dev}"}},
                        "dup": {"command": "from-local"}
                    },
                    "disabledMcpServers": ["Local_Srv"]
                },
                "/elsewhere": {"mcpServers": {"other": {"command": "nope"}}}
            }
        })
        .to_string(),
    );
    t.write(
        "project/.mcp.json",
        &json!({
            "mcpServers": {
                "GitHub": {
                    "type": "http",
                    "url": "https://api.example.test/mcp/",
                    "headers": {"Authorization": "Bearer ${GH_TOKEN}", "X-Org": "simstech"},
                    "oauth": {"clientId": "cid", "scopes": "repo read:org", "callbackPort": 8765}
                },
                "fs": {"command": "npx", "args": ["-y", "server-fs", "${ROOT:-/srv}"], "timeout": 90000},
                "helper": {"type": "sse", "url": "https://sse.example.test/sse", "headersHelper": "/bin/print-headers"},
                "dup": {"command": "from-project"},
                "sock": {"type": "ws", "url": "wss://ws.example.test"}
            }
        })
        .to_string(),
    );
}

#[test]
fn claude_code_dry_run_then_import() {
    let t = T::new("claude");
    claude_fixture(&t);

    // Dry run: everything planned, nothing written.
    let out = t.run(&["mcp", "import", "--from", "claude", "--dry-run", "--json"]);
    let v = ok(&out).json();
    assert_eq!(v["dry_run"], true);
    assert!(!t.user_file().exists());
    let labels: Vec<&str> = v["sources"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s["label"].as_str().unwrap())
        .collect();
    assert!(
        labels[0].starts_with("local scope in ~/.claude.json"),
        "{labels:?}"
    );
    assert!(labels[1].starts_with("project ") && labels[1].ends_with(".mcp.json"));
    assert!(labels[2].starts_with("user scope in ~/.claude.json"));

    let gh = row(&v, "GitHub");
    assert_eq!(gh["name"], "github");
    assert_eq!(gh["action"], "add");
    assert_eq!(gh["type"], "http");
    let tb = toml_of(gh);
    assert_eq!(tb["url"].as_str(), Some("https://api.example.test/mcp/"));
    assert_eq!(
        tb["headers"]["Authorization"].as_str(),
        Some("Bearer ${env:GH_TOKEN}")
    );
    assert_eq!(
        tb["oauth"]["scopes"],
        toml::Value::Array(vec!["repo".into(), "read:org".into()])
    );
    assert_eq!(tb["oauth"]["callback_port"].as_integer(), Some(8765));
    assert!(
        gh["toml"]
            .as_str()
            .unwrap()
            .contains("# Imported from Claude Code")
    );

    let fs = toml_of(row(&v, "fs"));
    assert_eq!(fs["args"][2].as_str(), Some("${env:ROOT:-/srv}"));
    assert_eq!(fs["call_timeout_secs"].as_integer(), Some(90));

    let local = row(&v, "Local_Srv");
    assert_eq!(local["name"], "local-srv");
    let lt = toml_of(local);
    assert_eq!(lt["enabled"].as_bool(), Some(false), "disabledMcpServers");
    assert_eq!(lt["env"]["MODE"].as_str(), Some("${env:MODE:-dev}"));

    let helper = row(&v, "helper");
    assert_eq!(helper["type"], "sse");
    assert!(
        helper["warnings"][0]
            .as_str()
            .unwrap()
            .contains("headersHelper")
    );

    // dup: local wins, project and user are shadowed.
    let dups = rows_named(&v, "dup");
    assert_eq!(dups.len(), 3);
    assert_eq!(dups[0]["action"], "add");
    assert_eq!(toml_of(dups[0])["command"].as_str(), Some("from-local"));
    assert_eq!(dups[1]["action"], "shadowed");
    assert_eq!(dups[2]["action"], "shadowed");
    // Another project's local servers are not ours.
    assert!(rows_named(&v, "other").is_empty());

    let ws = row(&v, "sock");
    assert_eq!(ws["action"], "error");
    assert!(ws["reason"].as_str().unwrap().contains("`ws`"));

    // The text preview says the same.
    let out = t.run(&["mcp", "import", "--from", "claude", "--dry-run"]);
    ok(&out);
    assert!(out.stdout.contains("[mcp.github]"), "{}", out.stdout);
    assert!(out.stdout.contains("`sock`: can't import"));
    assert!(out.stdout.contains("Dry run: nothing was written."));

    // A real import refuses while a selected server can't be imported, and names the way out.
    let out = t.run(&["mcp", "import", "--from", "claude", "--yes"]);
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("--only"), "{}", out.stderr);
    assert!(!t.user_file().exists());

    let only = "GitHub,fs,helper,Local_Srv,dup,user-srv";
    let out = t.run(&[
        "mcp", "import", "--from", "claude", "--only", only, "--yes", "--json",
    ]);
    let v = ok(&out).json();
    assert_eq!(v["imported"], 6, "{v}");
    assert_eq!(row(&v, "sock")["action"], "not-selected");
    let text = read(&t.user_file());
    for n in ["github", "fs", "helper", "local-srv", "dup", "user-srv"] {
        assert!(text.contains(&format!("[mcp.{n}]")), "{text}");
    }
    let list = ok(&t.run(&["mcp", "list", "--no-health", "--json"])).json();
    let names: Vec<&str> = list["servers"]
        .as_array()
        .or_else(|| list.as_array())
        .unwrap()
        .iter()
        .map(|s| s["name"].as_str().unwrap())
        .collect();
    assert!(
        names.contains(&"github") && names.contains(&"local-srv"),
        "{list}"
    );

    // Importing again changes nothing.
    let out = t.run(&[
        "mcp", "import", "--from", "claude", "--only", only, "--yes", "--json",
    ]);
    let v = ok(&out).json();
    assert_eq!(v["imported"], 0);
    assert_eq!(row(&v, "GitHub")["action"], "unchanged");
    assert_eq!(read(&t.user_file()), text);
}

#[test]
fn opencode_vscode_cursor_and_desktop_formats() {
    let t = T::new("formats");
    // opencode: JSONC, {env:}, local command list, remote oauth false, timeout = startup.
    t.write(
        "project/opencode.jsonc",
        r#"{
  // opencode allows comments
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "ctx7": {"type": "remote", "url": "https://mcp.ctx.test/mcp",
             "headers": {"X-Api-Key": "{env:CTX_KEY}"}, "oauth": false, "timeout": 2000},
    "local": {"type": "local", "command": ["bunx", "srv", "--dir", "{env:DIR}"],
              "environment": {"LEVEL": "info"}, "enabled": false},
  },
}"#,
    );
    t.write(
        "home/.config/opencode/opencode.json",
        &json!({"mcp": {"global-one": {"type": "local", "command": ["g"]}}}).to_string(),
    );
    // VS Code: servers + inputs, ${workspaceFolder}, envFile, dev dropped.
    t.write(
        "project/.vscode/mcp.json",
        r#"{
  "inputs": [{"type": "promptString", "id": "api-key", "description": "Perplexity key", "password": true}],
  "servers": {
    "Perplexity": {"type": "stdio", "command": "npx", "args": ["-y", "pplx"],
                   "env": {"PERPLEXITY_API_KEY": "${input:api-key}"}, "envFile": "${workspaceFolder}/.env",
                   "dev": {"watch": "src/**"}},
    "remote": {"type": "http", "url": "https://r.test/mcp", "headers": {"X-Home": "${userHome}"}}
  }
}"#,
    );
    // Cursor: no type, ${env:}, ${workspaceFolder}, auth.
    t.write(
        "project/.cursor/mcp.json",
        &json!({"mcpServers": {
            "db": {"command": "db-mcp", "args": ["--root", "${workspaceFolder}"], "env": {"DB_URL": "${env:DB_URL}"}},
            "api": {"url": "https://api.cursor.test/mcp", "auth": {"CLIENT_ID": "cid", "scopes": ["read"]}}
        }})
        .to_string(),
    );
    t.write(
        "home/.cursor/mcp.json",
        &json!({"mcpServers": {"db": {"command": "global-db"}, "g": {"command": "g2"}}})
            .to_string(),
    );
    // Claude Desktop: values are literal.
    let desktop = if cfg!(target_os = "macos") {
        "home/Library/Application Support/Claude/claude_desktop_config.json"
    } else {
        "home/.config/Claude/claude_desktop_config.json"
    };
    t.write(
        desktop,
        &json!({"mcpServers": {"Desk Top": {"command": "uvx", "args": ["tool", "${NOT_A_REF}"]}}})
            .to_string(),
    );

    let plan =
        |from: &str| ok(&t.run(&["mcp", "import", "--from", from, "--dry-run", "--json"])).json();

    let v = plan("opencode");
    let c = toml_of(row(&v, "ctx7"));
    assert_eq!(c["type"].as_str(), Some("http"));
    assert_eq!(c["headers"]["X-Api-Key"].as_str(), Some("${env:CTX_KEY:-}"));
    assert_eq!(c["auth"].as_str(), Some("none"));
    assert_eq!(c["startup_timeout_secs"].as_integer(), Some(2));
    let l = toml_of(row(&v, "local"));
    assert_eq!(l["command"].as_str(), Some("bunx"));
    assert_eq!(l["args"][2].as_str(), Some("${env:DIR:-}"));
    assert_eq!(l["env"]["LEVEL"].as_str(), Some("info"));
    assert_eq!(l["enabled"].as_bool(), Some(false));
    assert_eq!(row(&v, "global-one")["action"], "add");

    let v = plan("vscode");
    let p = row(&v, "Perplexity");
    assert_eq!(p["name"], "perplexity");
    let pt = toml_of(p);
    assert_eq!(
        pt["secret_env"],
        toml::Value::Array(vec!["PERPLEXITY_API_KEY".into()])
    );
    assert!(pt.get("env").is_none());
    assert_eq!(pt["env_file"].as_str(), Some("${repo}/.env"));
    assert_eq!(
        p["secrets"][0]["account"],
        "mcp:perplexity:env:PERPLEXITY_API_KEY"
    );
    assert_eq!(p["secrets"][0]["from"], "asked");
    assert_eq!(p["asks"][0]["hidden"], true);
    assert!(
        p["asks"][0]["prompt"]
            .as_str()
            .unwrap()
            .contains("Perplexity key")
    );
    assert!(p["warnings"][0].as_str().unwrap().contains("`dev`"));
    assert_eq!(
        toml_of(row(&v, "remote"))["headers"]["X-Home"].as_str(),
        Some("${home}")
    );

    let v = plan("cursor");
    let dbs = rows_named(&v, "db");
    assert_eq!(dbs[0]["action"], "add");
    assert_eq!(dbs[1]["action"], "shadowed");
    let d = toml_of(dbs[0]);
    assert_eq!(d["type"].as_str(), Some("stdio"));
    assert_eq!(d["args"][1].as_str(), Some("${repo}"));
    assert_eq!(d["env"]["DB_URL"].as_str(), Some("${env:DB_URL:-}"));
    let a = toml_of(row(&v, "api"));
    assert_eq!(a["oauth"]["client_id"].as_str(), Some("cid"));

    let v = plan("claude-desktop");
    let dt = row(&v, "Desk Top");
    assert_eq!(dt["name"], "desk-top");
    assert_eq!(toml_of(dt)["args"][1].as_str(), Some("$${NOT_A_REF}"));

    // A named file in any of the formats.
    let v = ok(&t.run(&[
        "mcp",
        "import",
        "--from",
        ".cursor/mcp.json",
        "--dry-run",
        "--json",
    ]))
    .json();
    assert_eq!(v["sources"][0]["source"], "cursor");

    // No --from without a terminal: the sources found, and how to choose.
    let out = t.run(&["mcp", "import"]);
    assert_eq!(out.code, 2);
    for s in ["opencode", "vscode", "cursor", "claude-desktop"] {
        assert!(out.stderr.contains(s), "{}", out.stderr);
    }
    // --dry-run without --from previews them all.
    let v = ok(&t.run(&["mcp", "import", "--dry-run", "--json"])).json();
    assert!(v["sources"].as_array().unwrap().len() >= 6, "{v}");

    // Cursor into the repo config: written there and trusted for the importer.
    let out = t.run(&[
        "mcp", "import", "--from", "cursor", "--scope", "repo", "--yes", "--json",
    ]);
    let v = ok(&out).json();
    assert_eq!(v["imported"], 3, "{v}");
    assert_eq!(row(&v, "db")["result"]["trusted"], true);
    let repo = read(&t.repo_file());
    assert!(
        repo.contains("[mcp.db]") && repo.contains("[mcp.api]"),
        "{repo}"
    );
    assert!(!t.user_file().exists() || !read(&t.user_file()).contains("[mcp.db]"));
    let trust = ok(&t.run(&["mcp", "trust", "--json"])).json();
    let s = trust.to_string();
    assert!(s.contains("\"db\"") && s.contains("trusted"), "{s}");

    // VS Code input without a terminal or --secret-stdin: refused before writing.
    let out = t.run_with(
        &[
            "mcp",
            "import",
            "--from",
            "vscode",
            "--only",
            "Perplexity",
            "--yes",
        ],
        &[],
        None,
    );
    assert_ne!(out.code, 0);
    assert!(!read(&t.user_file()).contains("perplexity"));
}

#[test]
fn clashes_rename_only_force_and_confirmation() {
    let t = T::new("clash");
    claude_fixture(&t);
    std::fs::write(
        t.user_file(),
        "# mine\n[mcp.github]\ntype = \"stdio\"\ncommand = \"my-own\"\n",
    )
    .unwrap();

    let v = ok(&t.run(&["mcp", "import", "--from", "claude", "--dry-run", "--json"])).json();
    assert_eq!(row(&v, "GitHub")["name"], "github-2");

    let v = ok(&t.run(&[
        "mcp",
        "import",
        "--from",
        "claude",
        "--dry-run",
        "--json",
        "--rename",
        "GitHub=gh",
    ]))
    .json();
    assert_eq!(row(&v, "GitHub")["name"], "gh");

    let v = ok(&t.run(&[
        "mcp",
        "import",
        "--from",
        "claude",
        "--dry-run",
        "--json",
        "--force",
    ]))
    .json();
    assert_eq!(row(&v, "GitHub")["name"], "github");
    assert_eq!(row(&v, "GitHub")["action"], "replace");

    let out = t.run(&[
        "mcp",
        "import",
        "--from",
        "claude",
        "--only",
        "nope",
        "--dry-run",
    ]);
    assert_eq!(out.code, 2);

    // Without a terminal and without --yes: the plan, then a refusal.
    let out = t.run(&["mcp", "import", "--from", "claude", "--only", "fs"]);
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(out.stderr.contains("--yes"));
    assert!(!read(&t.user_file()).contains("[mcp.fs]"));

    // Scripted answers: no to all, then pick one.
    let out = t.run_with(
        &["mcp", "import", "--from", "claude", "--only", "fs,user-srv"],
        &[("AMP_HARNESS_PROMPTS", "stdin")],
        Some("n\nfs\n"),
    );
    ok(&out);
    let text = read(&t.user_file());
    assert!(
        text.contains("[mcp.fs]") && !text.contains("[mcp.user-srv]"),
        "{text}"
    );
    assert!(text.starts_with("# mine"), "comments kept: {text}");

    // --force replaces the person's own github.
    let out = t.run(&[
        "mcp", "import", "--from", "claude", "--only", "GitHub", "--force", "--yes",
    ]);
    ok(&out);
    let text = read(&t.user_file());
    assert!(
        !text.contains("my-own") && text.contains("api.example.test"),
        "{text}"
    );
    assert!(
        t.dir
            .join("home/.config/amp-harness/config.toml.bak")
            .exists()
    );
}

#[test]
fn secrets_go_to_the_keychain_only() {
    let t = T::new("secrets");
    t.write(
        "project/.mcp.json",
        &json!({"mcpServers": {
            "lit": {"type": "http", "url": "https://lit.test/mcp", "headers": {"Authorization": format!("Bearer {PLANTED}")}},
            "envy": {"command": "envy", "env": {"GITHUB_TOKEN": PLANTED_ENV, "PLAIN": "1"}},
            "clean": {"command": "clean"}
        }})
        .to_string(),
    );
    let v = ok(&t.run(&["mcp", "import", "--from", "claude", "--dry-run", "--json"])).json();
    let lit = row(&v, "lit");
    assert_eq!(lit["secrets"][0]["account"], "mcp:lit:header:authorization");
    assert_eq!(lit["secrets"][0]["from"], "literal");
    assert_eq!(
        toml_of(lit)["secret_headers"],
        toml::Value::Array(vec!["Authorization".into()])
    );
    let envy = toml_of(row(&v, "envy"));
    assert_eq!(
        envy["secret_env"],
        toml::Value::Array(vec!["GITHUB_TOKEN".into()])
    );
    assert_eq!(envy["env"]["PLAIN"].as_str(), Some("1"));

    let out = t.run(&["mcp", "import", "--from", "claude", "--dry-run"]);
    ok(&out);
    assert!(out.stdout.contains("keychain: header Authorization -> mcp:lit:header:authorization (moved out of the source file)"), "{}", out.stdout);
    for s in [PLANTED, PLANTED_ENV] {
        no_secret(&out, s);
    }

    // The file credential store has no keychain: refused before anything is written.
    let out = t.run(&["mcp", "import", "--from", "claude", "--yes", "--json"]);
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(
        out.stderr.contains("nothing was written") && out.stderr.contains("lit"),
        "{}",
        out.stderr
    );
    assert!(!t.user_file().exists());
    // Without the servers that need it, the rest imports.
    ok(&t.run(&[
        "mcp", "import", "--from", "claude", "--only", "clean", "--yes",
    ]));
    for s in [PLANTED, PLANTED_ENV] {
        no_secret(&out, s);
        t.assert_no_file_has(s);
    }
}

/// Opt-in (macOS, `AMP_TEST_KEYCHAIN=1`): a literal secret and a VS Code password input land in the
/// real keychain under a test-only service, show as stored, and are removed with the server.
#[test]
fn keychain_import_opt_in() {
    if !(cfg!(target_os = "macos") && std::env::var("AMP_TEST_KEYCHAIN").is_ok_and(|v| v == "1")) {
        eprintln!("skipped: set AMP_TEST_KEYCHAIN=1 to run the real-keychain case (macOS)");
        return;
    }
    let service = format!("amp-harness-test-mcp-import-{}", std::process::id());
    const ACCOUNTS: [&str; 2] = ["mcp:lit:header:authorization", "mcp:pplx:env:PPLX_KEY"];
    struct Cleanup(String);
    impl Drop for Cleanup {
        fn drop(&mut self) {
            for account in ACCOUNTS {
                let _ = Command::new("security")
                    .args(["delete-generic-password", "-s", &self.0, "-a", account])
                    .output();
            }
        }
    }
    let _cleanup = Cleanup(service.clone());
    let mut t = T::new("keychain");
    t.keychain = Some(service.clone());
    // What the servers receive is checked through amp-harness itself (`mcp test`): reading the
    // items with `security -w` would raise a keychain dialog, since amp-harness created them.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let seen = std::sync::Arc::new(std::sync::Mutex::new(None::<String>));
    let seen2 = seen.clone();
    std::thread::spawn(move || {
        use std::io::{BufRead, BufReader};
        for stream in listener.incoming().flatten() {
            let mut reader = BufReader::new(stream.try_clone().unwrap());
            let mut line = String::new();
            while reader.read_line(&mut line).is_ok_and(|n| n > 2) {
                if let Some(v) = line.to_ascii_lowercase().strip_prefix("authorization:") {
                    let start = line.len() - v.len();
                    seen2
                        .lock()
                        .unwrap()
                        .get_or_insert(line[start..].trim().to_owned());
                }
                line.clear();
            }
            let mut s = stream;
            let _ = s.write_all(
                b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n",
            );
        }
    });
    let claude = t.write(
        "project/fixtures/claude.json",
        &json!({"mcpServers": {"lit": {"type": "http", "url": format!("http://127.0.0.1:{port}/mcp"),
            "headers": {"Authorization": format!("Bearer {PLANTED}")}}}})
        .to_string(),
    );
    let env_out = t.dir.join("env-out");
    let vscode = t.write(
        "project/.vscode/mcp.json",
        &json!({"inputs": [{"type": "promptString", "id": "k", "description": "key", "password": true}],
                "servers": {"pplx": {"command": "/bin/sh",
                    "args": ["-c", format!("printf %s \"$PPLX_KEY\" > '{}'", env_out.display())],
                    "env": {"PPLX_KEY": "${input:k}"}}}})
        .to_string(),
    );
    let out = t.run(&[
        "mcp",
        "import",
        "--from",
        claude.to_str().unwrap(),
        "--yes",
        "--json",
    ]);
    let v = ok(&out).json();
    assert_eq!(
        row(&v, "lit")["result"]["secrets_stored"],
        json!(["header Authorization"])
    );
    no_secret(&out, PLANTED);
    let out = t.run_with(
        &[
            "mcp",
            "import",
            "--from",
            vscode.to_str().unwrap(),
            "--yes",
            "--secret-stdin",
        ],
        &[],
        Some(&format!("{PLANTED_ENV}\n")),
    );
    ok(&out);
    no_secret(&out, PLANTED_ENV);
    for account in ACCOUNTS {
        // Attributes only (no -w): never a dialog.
        let found = Command::new("security")
            .args(["find-generic-password", "-s", &service, "-a", account])
            .output()
            .unwrap();
        assert!(found.status.success(), "{account} is not in the keychain");
    }
    for name in ["lit", "pplx"] {
        let out = t.run(&["mcp", "get", name, "--json", "--no-health"]);
        assert_eq!(ok(&out).json()["secrets"][0]["state"], "stored", "{name}");
        no_secret(&out, PLANTED);
        no_secret(&out, PLANTED_ENV);
        let out = t.run(&["mcp", "test", name]);
        no_secret(&out, PLANTED);
        no_secret(&out, PLANTED_ENV);
    }
    assert!(
        seen.lock().unwrap().as_deref() == Some(format!("Bearer {PLANTED}").as_str()),
        "the http server did not get the stored Authorization header"
    );
    let got = std::fs::read_to_string(&env_out).unwrap_or_default();
    let _ = std::fs::remove_file(&env_out);
    assert!(
        got == PLANTED_ENV,
        "the stdio server did not get the stored env value"
    );
    for name in ["lit", "pplx"] {
        ok(&t.run(&["mcp", "remove", name]));
    }
    for account in ACCOUNTS {
        let found = Command::new("security")
            .args(["find-generic-password", "-s", &service, "-a", account])
            .output()
            .unwrap();
        assert!(!found.status.success(), "{account} was not deleted");
    }
    t.assert_no_file_has(PLANTED);
    t.assert_no_file_has(PLANTED_ENV);
}
