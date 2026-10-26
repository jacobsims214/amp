//! #2232 (D10): `--config FILE` / `AMP_HARNESS_CONFIG` and `--config-dir DIR` /
//! `AMP_HARNESS_CONFIG_DIR` apply to every setting and every command, and the repo layer
//! `<repo>/.amp/harness/config.toml` loads with `[roles]`, `[mcp]` and `[models] profile` only.
//!
//! The trick: the default `~/.config/amp-harness/config.toml` holds values that would fail (a
//! zero `[workers] max_parallel`, a negative `[tools] bash_timeout_secs`, a manager on an unknown
//! provider, a theme file that does not exist), so a command only succeeds when it read the other
//! file for every table. Everything lives under cargo's target tmp dir (D13).

mod common;

use std::path::{Path, PathBuf};

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]
[providers.faux.models."faux-2"]

[roles.manager]
model = "faux/faux-1"
[roles.worker-backend]
model = "faux/faux-1"
"#;

/// The default config.toml: every table would break a command that read it.
const POISON: &str = r#"
[workers]
max_parallel = 0

[tools]
bash_timeout_secs = -1

[roles.manager]
model = "nowhere/never"

[tui]
theme = "/nonexistent/amp-harness-theme.json"
"#;

/// The file `--config` names: the faux models plus valid values for the same tables.
fn good_config() -> String {
    format!(
        "{FAUX}\n[workers]\nmax_parallel = 2\n\n[tools]\nbash_timeout_secs = 30\n\n[tui]\ntheme = \"light\"\n"
    )
}

/// A work dir (project 2) with the poisoned default config and a faux script that echoes the
/// system prompt.
fn setup(name: &str) -> PathBuf {
    let dir = workdir(name, Some(2));
    let default_dir = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&default_dir).unwrap();
    std::fs::write(default_dir.join("config.toml"), POISON).unwrap();
    std::fs::write(
        default_dir.join("AGENTS.md"),
        "DEFAULT-DIR-CONTEXT must not load.",
    )
    .unwrap();
    std::fs::write(
        dir.join("script.json"),
        json!([{"text": "{{system_prompt}}"}]).to_string(),
    )
    .unwrap();
    dir
}

async fn harness(dir: &Path, url: &str, args: &[&str], env: &[(&str, &str)]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let mut all = vec![
        (
            "AMP_HARNESS_FAUX_SCRIPT",
            script.to_str().unwrap().to_owned(),
        ),
        ("XDG_STATE_HOME", state.to_str().unwrap().to_owned()),
    ];
    all.extend(env.iter().map(|(k, v)| (*k, (*v).to_owned())));
    let env: Vec<(&str, &str)> = all.iter().map(|(k, v)| (*k, v.as_str())).collect();
    run(dir, url, args, &env).await
}

fn session(out: &Output) -> Value {
    let first = out.stdout.lines().next().unwrap_or_default();
    serde_json::from_str(first).unwrap_or_else(|e| panic!("not JSON ({e}): {}", out.stdout))
}

#[tokio::test]
async fn config_flag_and_env_apply_to_models_workers_tools_and_tui() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("cfgpaths-flag");
    let file = dir.join("elsewhere/my-config.toml");
    std::fs::create_dir_all(file.parent().unwrap()).unwrap();
    std::fs::write(&file, good_config()).unwrap();
    let f = file.to_str().unwrap();

    // Without --config the poisoned default file is read, and fails.
    let out = harness(&dir, &amp.url, &["run", "-p", "hi", "--json"], &[]).await;
    assert_ne!(out.code, 0, "{}", out.stdout);

    // Before the subcommand, after it, and as AMP_HARNESS_CONFIG: [roles] (the faux manager),
    // [workers] (the supervisor) and [tools] (bash timeout) all come from the named file.
    for (args, env) in [
        (vec!["--config", f, "run", "-p", "hi", "--json"], vec![]),
        (vec!["run", "-p", "hi", "--json", "--config", f], vec![]),
        (
            vec!["run", "-p", "hi", "--json"],
            vec![("AMP_HARNESS_CONFIG", f)],
        ),
    ] {
        let out = harness(&dir, &amp.url, &args, &env).await;
        assert_eq!(out.code, 0, "{args:?}: {}\n{}", out.stderr, out.stdout);
        let s = session(&out);
        assert_eq!(s["role"], "amp-manager");
        assert_eq!(s["provider"], "faux", "{args:?}");
        assert_eq!(s["model"], "faux-1");
    }

    // config show reads it too.
    let out = harness(
        &dir,
        &amp.url,
        &["--config", f, "config", "show", "--json"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = out.json();
    assert_eq!(report["config_file"], f);
    assert_eq!(report["config_found"], true);

    // [tui]: the chat's header renders with the named file's theme, not the default's missing one.
    let out = harness(
        &dir,
        &amp.url,
        &["debug", "render-chrome", "--plain", "--config", f],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let out = harness(&dir, &amp.url, &["debug", "render-chrome", "--plain"], &[]).await;
    assert_ne!(
        out.code, 0,
        "the default file's theme should fail: {}",
        out.stdout
    );

    // [workers] really is read from the named file: a bad value there is an error naming it.
    std::fs::write(&file, format!("{FAUX}\n[workers]\nmax_parallel = 0\n")).unwrap();
    let out = harness(&dir, &amp.url, &["--config", f, "run", "-p", "hi"], &[]).await;
    assert_ne!(out.code, 0);
    assert!(
        out.stderr.contains("my-config.toml") && out.stderr.contains("max_parallel"),
        "{}",
        out.stderr
    );

    // A file the person named must exist (as before #2232).
    let out = harness(
        &dir,
        &amp.url,
        &["--config", "/nonexistent/amp-harness.toml", "whoami"],
        &[],
    )
    .await;
    assert_ne!(out.code, 0);
    assert!(out.stderr.contains("does not exist"), "{}", out.stderr);
}

#[tokio::test]
async fn config_dir_moves_the_whole_user_layer() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("cfgpaths-dir");
    let layer = dir.join("layer");
    std::fs::create_dir_all(layer.join("skills/cfg-probe")).unwrap();
    std::fs::write(layer.join("config.toml"), good_config()).unwrap();
    std::fs::write(layer.join("AGENTS.md"), "LAYER-DIR-CONTEXT loads.").unwrap();
    std::fs::write(
        layer.join("skills/cfg-probe/SKILL.md"),
        "---\nname: cfg-probe\ndescription: A skill only the --config-dir layer has.\n---\nProbe.\n",
    )
    .unwrap();
    let d = layer.to_str().unwrap();

    for (args, env) in [
        (
            vec!["--config-dir", d, "run", "--print-system-prompt"],
            vec![],
        ),
        (
            vec!["run", "--print-system-prompt"],
            vec![("AMP_HARNESS_CONFIG_DIR", d)],
        ),
    ] {
        let out = harness(&dir, &amp.url, &args, &env).await;
        assert_eq!(out.code, 0, "{args:?}: {}", out.stderr);
        assert!(
            out.stdout.contains("LAYER-DIR-CONTEXT"),
            "AGENTS.md from the dir"
        );
        assert!(!out.stdout.contains("DEFAULT-DIR-CONTEXT"));
        assert!(
            out.stdout.contains("<name>cfg-probe</name>"),
            "skills/ from the dir"
        );
    }

    // credentials.json lives in the dir too; --config alone does not move it.
    let out = harness(
        &dir,
        &amp.url,
        &["--config-dir", d, "doctor", "--json"],
        &[],
    )
    .await;
    let report: Value = serde_json::from_str(&out.stdout).unwrap();
    assert_eq!(
        report["auth"]["credential_store"],
        format!("file {}", layer.join("credentials.json").display())
    );
    assert_eq!(
        report["models"]["config_file"],
        layer.join("config.toml").display().to_string()
    );
    let f = layer.join("config.toml");
    let out = harness(
        &dir,
        &amp.url,
        &["--config", f.to_str().unwrap(), "doctor", "--json"],
        &[],
    )
    .await;
    let report: Value = serde_json::from_str(&out.stdout).unwrap();
    assert_eq!(
        report["auth"]["credential_store"],
        format!(
            "file {}",
            dir.join("home/.config/amp-harness/credentials.json")
                .display()
        )
    );
}

#[tokio::test]
async fn writers_use_the_named_file() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("cfgpaths-writers", None);
    let default_file = dir.join("home/.config/amp-harness/config.toml");
    let file = dir.join("named.toml");
    let f = file.to_str().unwrap();
    let web = amp.url.trim_end_matches("/mcp").to_owned();

    let out = run(
        &dir,
        "",
        &[
            "--config",
            f,
            "server",
            "set",
            &amp.url,
            "--name",
            "probe",
            "--web-url",
            &web,
            "--yes",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let text = std::fs::read_to_string(&file).unwrap();
    assert!(text.contains(amp.url.as_str()), "{text}");
    assert!(
        !default_file.exists(),
        "the default file must stay untouched"
    );

    // `server` reads it back from the same file (flag after the subcommand).
    let out = run(&dir, "", &["server", "--json", "--config", f], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.json()["url"]["value"], amp.url.as_str());

    // --config-dir: config.toml inside a fresh dir.
    let layer = dir.join("layer");
    let out = run(
        &dir,
        "",
        &[
            "--config-dir",
            layer.to_str().unwrap(),
            "server",
            "set",
            &amp.url,
            "--web-url",
            &web,
            "--yes",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(layer.join("config.toml").is_file());
    assert!(!default_file.exists());
}

#[tokio::test]
async fn repo_layer_sets_roles_and_profile_only() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("cfgpaths-repo", Some(2));
    let user = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&user).unwrap();
    std::fs::write(user.join("config.toml"), FAUX).unwrap();
    std::fs::write(dir.join("script.json"), json!([{"text": "ok"}]).to_string()).unwrap();
    let repo = dir.join("project/.amp/harness");
    std::fs::create_dir_all(&repo).unwrap();
    let repo_file = repo.join("config.toml");
    std::fs::write(
        &repo_file,
        "[roles.manager]\nmodel = \"faux/faux-2\"\n\n[mcp.probe]\ntype = \"http\"\nurl = \"https://example.test/mcp\"\n",
    )
    .unwrap();

    // #2310: the repo's [roles] count only once trusted.
    let out = harness(&dir, &amp.url, &["config", "show", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = out.json();
    let manager = report["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == "manager")
        .unwrap()
        .clone();
    assert_eq!(manager["model"], "faux-1", "{manager}");
    let out = harness(&dir, &amp.url, &["trust", "role:manager"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);

    // Trusted, the repo's [roles] beat the user's; [mcp] is accepted (the MCP layer reads it).
    let out = harness(&dir, &amp.url, &["config", "show", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = out.json();
    let canon = std::fs::canonicalize(&repo_file).unwrap();
    let shown = report["repo_file"].as_str().unwrap();
    assert!(
        shown == repo_file.display().to_string() || shown == canon.display().to_string(),
        "{shown}"
    );
    let manager = report["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == "manager")
        .unwrap();
    assert_eq!(manager["model"], "faux-2");
    assert!(
        manager["source"].as_str().unwrap().starts_with("repo "),
        "{manager}"
    );
    let out = harness(&dir, &amp.url, &["run", "-p", "hi", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(session(&out)["model"], "faux-2");

    // Anything else is an error naming the key and the file.
    for (text, key) in [
        ("[server]\nurl = \"https://evil.test/mcp\"\n", "`server`"),
        ("[workers]\nmax_parallel = 9\n", "`workers`"),
        ("[models]\ninherit = \"faux/faux-2\"\n", "`models.inherit`"),
    ] {
        std::fs::write(&repo_file, text).unwrap();
        for args in [&["config", "show"][..], &["run", "-p", "hi"][..]] {
            let out = harness(&dir, &amp.url, args, &[]).await;
            assert_ne!(out.code, 0, "{text}");
            assert!(
                out.stderr.contains(key) && out.stderr.contains(".amp/harness/config.toml"),
                "{args:?} {text}: {}",
                out.stderr
            );
        }
    }
}
