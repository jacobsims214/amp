//! #2164: `amp-harness server` (show / set / use / remove) and the chat's `/server`, against
//! in-process mock AMPs with a temp HOME under cargo's target dir (D13). Nothing here touches the
//! person's own config.toml or keychain (the file credential store is used).

mod common;

use std::{path::Path, process::Stdio, time::Duration};

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};
use tokio::io::AsyncWriteExt;

/// A token-shaped value that must never be written anywhere.
const TOKEN: &str = "amp_test_2164_never_write_me";

fn config_path(dir: &Path) -> std::path::PathBuf {
    dir.join("home/.config/amp-harness/config.toml")
}

fn read_config(dir: &Path) -> String {
    std::fs::read_to_string(config_path(dir)).unwrap_or_default()
}

fn table(dir: &Path) -> toml::Table {
    toml::from_str(&read_config(dir)).unwrap()
}

/// `amp-harness server …` with no AMP_MCP_URL (blank counts as unset), so config.toml decides.
async fn server(dir: &Path, args: &[&str], env: &[(&str, &str)]) -> Output {
    let mut full = vec!["server"];
    full.extend_from_slice(args);
    run(dir, "", &full, env).await
}

/// The same with `stdin` as the answers (`AMP_HARNESS_PROMPTS=stdin`).
async fn server_answering(dir: &Path, args: &[&str], stdin: &str) -> Output {
    let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
    cmd.arg("server")
        .args(args)
        .current_dir(dir.join("project"))
        .env("HOME", dir.join("home"))
        .env("XDG_CONFIG_HOME", dir.join("home/.config"))
        .env("AMP_CREDENTIAL_STORE", "file")
        .env("AMP_HARNESS_CEILING_DIR", dir)
        .env("AMP_HARNESS_PROMPTS", "stdin")
        .env_remove("AMP_MCP_URL")
        .env_remove("AMP_TOKEN")
        .env_remove("AMP_WEB_URL")
        .env_remove("AMP_CA_FILE")
        .env_remove("AMP_MCP_PROTOCOL")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = cmd.spawn().expect("spawn amp-harness");
    let mut input = child.stdin.take().unwrap();
    input.write_all(stdin.as_bytes()).await.unwrap();
    drop(input);
    let out = tokio::time::timeout(Duration::from_secs(60), child.wait_with_output())
        .await
        .expect("amp-harness finished within 60 s")
        .unwrap();
    Output {
        code: out.status.code().unwrap_or(-1),
        stdout: String::from_utf8(out.stdout).unwrap(),
        stderr: String::from_utf8(out.stderr).unwrap(),
    }
}

/// A mock AMP whose own origin also serves the web UI (an HTML `/`), as hosted AMP does.
async fn spawn_with_ui(mock: MockAmp) -> common::Running {
    // `spawn` serves only /mcp; serve the same app plus `/` ourselves.
    let app = common::app(&mock, None).route(
        "/",
        axum::routing::get(|| async {
            (
                [(axum::http::header::CONTENT_TYPE, "text/html; charset=utf-8")],
                "<!doctype html><title>AMP</title>",
            )
        }),
    );
    common::serve(app, mock).await
}

fn ok(out: &Output) {
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
}

#[tokio::test]
async fn set_show_use_and_remove_keep_every_other_setting() {
    let alpha = spawn_with_ui(MockAmp::new()).await;
    let beta = spawn(MockAmp::new(), None).await;
    let dir = workdir("server-set-use", None);
    let before = "# my settings\n[tui]\ntheme = \"light\" # keep me\n\n[server]\nurl = \"http://127.0.0.1:9/mcp\"\n";
    std::fs::create_dir_all(config_path(&dir).parent().unwrap()).unwrap();
    std::fs::write(config_path(&dir), before).unwrap();

    // set: checked (whoami), saved, made active; the old unnamed server is kept; the web UI is
    // the server's own origin, so no web_url is written.
    let alpha_host = alpha.url.trim_end_matches("/mcp").to_owned();
    let out = server(&dir, &["set", &alpha_host, "--name", "alpha", "--yes"], &[]).await;
    ok(&out);
    for want in [
        format!("Saved AMP server `alpha`: {}", alpha.url),
        "(the server's own)".to_owned(),
        "kept your previous server as `local`".to_owned(),
        "Signed in to 127.0.0.1:".to_owned(),
        "as local-dev@example.com".to_owned(),
    ] {
        assert!(
            out.stdout.contains(&want),
            "missing {want:?}:\n{}",
            out.stdout
        );
    }
    assert_eq!(alpha.mock.calls_to("amp_whoami").len(), 1);
    let t = table(&dir);
    assert_eq!(t["server"]["url"].as_str(), Some(alpha.url.as_str()));
    assert_eq!(t["server"]["name"].as_str(), Some("alpha"));
    assert!(t["server"].get("web_url").is_none());
    assert_eq!(
        t["servers"]["alpha"]["url"].as_str(),
        Some(alpha.url.as_str())
    );
    assert_eq!(
        t["servers"]["local"]["url"].as_str(),
        Some("http://127.0.0.1:9/mcp")
    );
    let text = read_config(&dir);
    assert!(text.starts_with("# my settings\n"), "{text}");
    assert!(text.contains("theme = \"light\" # keep me"), "{text}");
    let backup = config_path(&dir).with_extension("toml.bak");
    assert_eq!(std::fs::read_to_string(&backup).unwrap(), before);

    // set with a web UI elsewhere (docker compose), as JSON.
    let out = server(
        &dir,
        &[
            "set",
            &beta.url,
            "--name",
            "beta",
            "--web-url",
            "http://localhost:5999/",
            "--yes",
            "--json",
        ],
        &[],
    )
    .await;
    ok(&out);
    let v = out.json();
    assert_eq!(v["status"], "ok");
    assert_eq!(v["server"]["web_url"], "http://localhost:5999");
    assert_eq!(v["user"], "local-dev@example.com");
    assert_eq!(
        table(&dir)["server"]["web_url"].as_str(),
        Some("http://localhost:5999")
    );

    // show: where each setting comes from, and the saved servers.
    let out = server(&dir, &["--json"], &[]).await;
    ok(&out);
    let v = out.json();
    assert_eq!(v["name"], "beta");
    assert_eq!(v["url"]["value"], beta.url.as_str());
    assert!(
        v["url"]["source"].as_str().unwrap().starts_with("config "),
        "{v}"
    );
    assert_eq!(v["web_url"]["value"], "http://localhost:5999");
    let names: Vec<&str> = v["saved"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["alpha", "beta", "local"]);
    // AMP_MCP_URL wins, and show says so.
    let out = server(&dir, &[], &[("AMP_MCP_URL", alpha.url.as_str())]).await;
    ok(&out);
    assert!(out.stdout.contains("AMP server: alpha"), "{}", out.stdout);
    assert!(
        out.stdout.contains("AMP_MCP_URL is set and wins"),
        "{}",
        out.stdout
    );

    // use: back to alpha; beta's web_url leaves [server].
    let out = server(&dir, &["use", "alpha", "--yes"], &[]).await;
    ok(&out);
    assert!(
        out.stdout.contains("Now using AMP server `alpha`"),
        "{}",
        out.stdout
    );
    let t = table(&dir);
    assert_eq!(t["server"]["url"].as_str(), Some(alpha.url.as_str()));
    assert!(t["server"].get("web_url").is_none(), "{t:?}");
    let out = server(&dir, &["show"], &[]).await;
    ok(&out);
    assert!(out.stdout.contains("* alpha"), "{}", out.stdout);
    assert!(out.stdout.contains("  beta"), "{}", out.stdout);

    // Unknown names are usage errors that list the saved ones.
    let out = server(&dir, &["use", "nope"], &[]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(
        out.stderr.contains("saved: alpha, beta, local"),
        "{}",
        out.stderr
    );

    // remove.
    let out = server(&dir, &["remove", "local"], &[]).await;
    ok(&out);
    assert!(table(&dir)["servers"].get("local").is_none());
    assert_eq!(
        table(&dir)["tui"]["theme"].as_str(),
        Some("light"),
        "other settings survive every edit"
    );
}

#[tokio::test]
async fn set_saves_nothing_for_a_server_that_does_not_answer() {
    let dir = workdir("server-unreachable", None);
    let out = server(&dir, &["set", "http://127.0.0.1:9/mcp", "--yes"], &[]).await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    // #2178: plain words, no rmcp/reqwest text.
    assert!(
        out.stderr
            .contains("Can't reach AMP at 127.0.0.1:9 (connection refused), so nothing was saved"),
        "{}",
        out.stderr
    );
    assert!(
        !out.stderr.contains("rmcp") && !out.stderr.contains("::"),
        "{}",
        out.stderr
    );
    assert!(!config_path(&dir).exists());

    // Bad input never gets as far as connecting.
    let out = server(&dir, &["set", "http://127.0.0.1:9", "--name", "a b"], &[]).await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert!(!config_path(&dir).exists());
}

#[tokio::test]
async fn a_server_that_needs_a_login_is_saved_and_offers_one() {
    let amp = spawn(MockAmp::new(), Some(TOKEN)).await;
    let dir = workdir("server-login", None);

    // No token, no terminal: saved, with the login to run.
    let out = server(
        &dir,
        &["set", &amp.url, "--name", "secure", "--yes", "--json"],
        &[],
    )
    .await;
    ok(&out);
    let v = out.json();
    assert_eq!(v["status"], "login_needed");
    assert!(
        v["warnings"][0]
            .as_str()
            .unwrap()
            .contains("run `amp-harness login`"),
        "{v}"
    );
    assert_eq!(table(&dir)["server"]["name"].as_str(), Some("secure"));

    // In a terminal it offers the browser login; "n" declines.
    let out = server_answering(&dir, &["use", "secure"], "n\n").await;
    ok(&out);
    assert!(out.stderr.contains("needs you to log in"), "{}", out.stderr);
    assert!(
        out.stderr.contains("Log in with your browser now?"),
        "{}",
        out.stderr
    );

    // A refused static token is reported by its variable, never by its value.
    let out = server(
        &dir,
        &["set", &amp.url, "--name", "secure", "--yes"],
        &[("AMP_TOKEN", "amp_wrong_2164")],
    )
    .await;
    ok(&out);
    assert!(
        out.stderr.contains("refused the token in AMP_TOKEN"),
        "{}",
        out.stderr
    );
    // The right one works and is never written.
    let out = server(
        &dir,
        &["set", &amp.url, "--name", "secure", "--yes", "--json"],
        &[("AMP_TOKEN", TOKEN)],
    )
    .await;
    ok(&out);
    assert_eq!(out.json()["status"], "ok");
    for entry in std::fs::read_dir(config_path(&dir).parent().unwrap()).unwrap() {
        let text = std::fs::read_to_string(entry.unwrap().path()).unwrap_or_default();
        assert!(!text.contains(TOKEN) && !text.contains("amp_wrong_2164"));
    }
    assert!(!out.stdout.contains(TOKEN) && !out.stderr.contains(TOKEN));
}

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

/// The chat switches servers without a restart: `/server` lists them, `/server two` reconnects
/// the session, the model's next AMP call goes to the new server, its tool list follows the new
/// server's, and config.toml makes `two` the active one.
#[tokio::test]
async fn the_chat_switches_servers_without_a_restart() {
    let one = spawn(MockAmp::new(), None).await;
    let mut two_mock = MockAmp::new();
    two_mock.omit = vec!["amp_kb_write"];
    let two = spawn(two_mock, None).await;
    let dir = workdir("server-chat-switch", Some(2));
    let config = format!(
        "{FAUX_CONFIG}\n[server]\nname = \"one\"\nurl = \"{0}\"\n\n[servers.one]\nurl = \"{0}\"\n\n\
         [servers.two]\nurl = \"{1}\"\nweb_url = \"http://localhost:5998\"\n",
        one.url, two.url
    );
    std::fs::create_dir_all(config_path(&dir).parent().unwrap()).unwrap();
    std::fs::write(config_path(&dir), &config).unwrap();
    let script = dir.join("script.json");
    std::fs::write(
        &script,
        json!([
            {"text": "Checking.", "tool_calls": [{"name": "amp_whoami", "id": "call-1"}]},
            {"text": "Done."}
        ])
        .to_string(),
    )
    .unwrap();
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        "",
        &[
            "--snapshot",
            "200x70",
            "--no-setup",
            "--input",
            "/server",
            "--input",
            "/server nope",
            "--input",
            "/server two",
            "--input",
            "who am I now?",
        ],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    ok(&out);
    let screen = &out.stdout;
    for want in [
        // /server lists the saved servers, the current one marked.
        format!("AMP server: `one` · {}", one.url),
        format!("* one  {}", one.url),
        format!("  two  {}  (web UI http://localhost:5998)", two.url),
        // An unknown name changes nothing.
        "/server: no saved server `nope` (saved: one, two)".to_owned(),
        // The switch.
        format!(
            "now on `two` · {} · signed in as local-dev@example.com",
            two.url
        ),
        "-amp_kb_write".to_owned(),
        "saved as the active server in".to_owned(),
        "project #2 (.amp.json) stays selected".to_owned(),
        "✓ AMP · whoami".to_owned(),
        "Done.".to_owned(),
    ] {
        assert!(screen.contains(&want), "missing {want:?} in\n{screen}");
    }
    // The model's call went to the new server; the old one never saw it.
    assert!(one.mock.calls_to("amp_whoami").is_empty());
    assert_eq!(
        two.mock.calls_to("amp_whoami").len(),
        2,
        "check + the model"
    );
    let t = table(&dir);
    assert_eq!(t["server"]["name"].as_str(), Some("two"));
    assert_eq!(t["server"]["url"].as_str(), Some(two.url.as_str()));
    assert_eq!(
        t["server"]["web_url"].as_str(),
        Some("http://localhost:5998")
    );
    assert_eq!(t["roles"]["manager"]["model"].as_str(), Some("faux/faux-1"));
}

/// The model's AMP tool list follows the server: after switching to a server without
/// `amp_kb_write`, the model can no longer call it (rpi answers "tool not found").
#[tokio::test]
async fn a_switched_session_only_has_the_new_servers_tools() {
    let one = spawn(MockAmp::new(), None).await;
    let mut two_mock = MockAmp::new();
    two_mock.omit = vec!["amp_kb_write"];
    let two = spawn(two_mock, None).await;
    let dir = workdir("server-chat-tools", Some(2));
    let config = format!(
        "{FAUX_CONFIG}\n[server]\nurl = \"{0}\"\n\n[servers.two]\nurl = \"{1}\"\n",
        one.url, two.url
    );
    std::fs::create_dir_all(config_path(&dir).parent().unwrap()).unwrap();
    std::fs::write(config_path(&dir), &config).unwrap();
    let script = dir.join("script.json");
    let call: Value = json!({"name": "amp_kb_write", "id": "call-1",
        "arguments": {"project_id": 2, "path": "a.md", "title": "a", "content": "a", "tags": ["a"]}});
    std::fs::write(
        &script,
        json!([
            {"text": "Writing.", "tool_calls": [call]},
            {"text": "Done."}
        ])
        .to_string(),
    )
    .unwrap();
    let state = dir.join("home/.local/state");
    let out = run(
        &dir,
        "",
        &[
            "--snapshot",
            "200x60",
            "--no-setup",
            "--input",
            "/server two",
            "--input",
            "write a doc",
        ],
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    ok(&out);
    // The unnamed first server was kept under a name when `two` became active.
    // (the notice wraps at this width)
    assert!(
        out.stdout.contains("server is kept as `local`)"),
        "{}",
        out.stdout
    );
    assert!(
        out.stdout.contains("Tool amp_kb_write not found"),
        "{}",
        out.stdout
    );
    assert!(one.mock.calls_to("amp_kb_write").is_empty());
    assert!(two.mock.calls_to("amp_kb_write").is_empty());
}
