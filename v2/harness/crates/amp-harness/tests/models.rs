//! `amp-harness models list|set|reset|profile` and `config check` (#2235): the resolved model per
//! role changes in the user and repo files, writes are safe merges with a backup, and every layer's
//! problems are reported with file and key. Temp HOMEs and repos live under target/ (D13).

mod common;

use std::path::{Path, PathBuf};

use common::{
    provider::{ProviderState, spawn_provider},
    run, workdir,
};
use serde_json::Value;

const URL: &str = "http://127.0.0.1:9/mcp";

fn user_file(dir: &Path) -> PathBuf {
    dir.join("home/.config/amp-harness/config.toml")
}

fn repo_file(dir: &Path) -> PathBuf {
    dir.join("project/.amp/harness/config.toml")
}

fn write(path: &Path, text: &str) {
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

fn read(path: &Path) -> String {
    std::fs::read_to_string(path).unwrap_or_default()
}

async fn amp(dir: &Path, args: &[&str], env: &[(&str, &str)]) -> common::Output {
    run(dir, URL, args, env).await
}

/// `config show --json`'s line for `role`.
async fn resolved(dir: &Path, role: &str, env: &[(&str, &str)]) -> Value {
    let out = amp(dir, &["config", "show", "--json"], env).await;
    let report = out.json();
    report["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == role)
        .cloned()
        .unwrap_or_else(|| panic!("role {role} in {report}"))
}

#[tokio::test]
async fn set_and_reset_change_the_resolved_model_in_the_user_file() {
    let dir = workdir("models-user", None);
    let user = user_file(&dir);
    write(
        &user,
        "# my settings\n[server]\nurl = \"http://localhost:8000/mcp\" # keep me\n\n[roles.reviewer]\nmodel = \"openrouter/xiaomi/mimo-v2.6-pro\"\nextra_body = { reasoning = { enabled = true } }\n",
    );
    let before = read(&user);

    let out = amp(
        &dir,
        &[
            "models",
            "set",
            "amp-reviewer",
            "anthropic/claude-opus-5-5",
            "--thinking",
            "high",
            "--json",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    let o = out.json();
    assert_eq!(o["changed"], true);
    assert_eq!(o["scope"], "user");
    assert_eq!(o["role"]["provider"], "anthropic");
    assert_eq!(o["role"]["model"], "claude-opus-5-5");
    assert_eq!(o["role"]["thinking"], "high");
    let warnings = o["warnings"].to_string();
    assert!(warnings.contains("extra_body"), "{warnings}");
    assert!(warnings.contains("no key"), "{warnings}");
    // Safe merge: comments and other settings stay, the old file is the backup.
    let text = read(&user);
    assert!(
        text.contains("# my settings") && text.contains("# keep me"),
        "{text}"
    );
    assert!(!text.contains("extra_body"), "{text}");
    assert_eq!(read(Path::new(o["backup"].as_str().unwrap())), before);

    let r = resolved(&dir, "reviewer", &[]).await;
    assert_eq!(
        (
            r["provider"].as_str(),
            r["model"].as_str(),
            r["thinking"].as_str()
        ),
        (Some("anthropic"), Some("claude-opus-5-5"), Some("high"))
    );
    assert!(r["source"].as_str().unwrap().starts_with("config "), "{r}");

    // The same again changes nothing.
    let o = amp(
        &dir,
        &[
            "models",
            "set",
            "reviewer",
            "anthropic/claude-opus-5-5",
            "--json",
        ],
        &[],
    )
    .await
    .json();
    assert_eq!(o["changed"], false);

    // A model the config doesn't define yet (no key, so not checked): its table is added.
    let out = amp(
        &dir,
        &[
            "models",
            "set",
            "kb-curator",
            "openrouter/openai/gpt-oss-120b",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    assert!(
        out.stdout
            .contains("added [providers.openrouter.models.\"openai/gpt-oss-120b\"]"),
        "{}",
        out.stdout
    );
    assert!(out.stderr.contains("not checked"), "{}", out.stderr);
    assert_eq!(
        resolved(&dir, "kb-curator", &[]).await["model"],
        "openai/gpt-oss-120b"
    );

    // Errors write nothing.
    let before = read(&user);
    for (args, needle) in [
        (
            &["models", "set", "reviewer", "nowhere/x"][..],
            "unknown provider `nowhere`",
        ),
        (
            &["models", "set", "reviewer", "no-slash"][..],
            "provider/model",
        ),
        (
            &[
                "models",
                "set",
                "reviewer",
                "anthropic/claude-opus-5-5",
                "--thinking",
                "huge",
            ][..],
            "unknown thinking level",
        ),
        (
            &["models", "set", "bad.role", "anthropic/claude-opus-5-5"][..],
            "not a role id",
        ),
    ] {
        let out = amp(&dir, args, &[]).await;
        assert_ne!(out.code, 0, "{args:?}");
        assert!(out.stderr.contains(needle), "{args:?}: {}", out.stderr);
        assert_eq!(read(&user), before, "{args:?}");
    }

    // Reset: back to the shipped default.
    let out = amp(&dir, &["models", "reset", "reviewer", "--json"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert_eq!(out.json()["changed"], true);
    let r = resolved(&dir, "reviewer", &[]).await;
    assert_eq!(r["model"], "z-ai/glm-5.3");
    assert_eq!(r["source"], "default");
    assert!(read(&user).contains("# keep me"));
    let o = amp(&dir, &["models", "reset", "reviewer", "--json"], &[])
        .await
        .json();
    assert_eq!(o["changed"], false);
    assert!(o["warnings"].to_string().contains("nothing to reset"));

    // `models list` shows the roles and their sources.
    let out = amp(&dir, &["models", "list"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(out.stdout.contains("kb-curator") && out.stdout.contains("SOURCE"));
    let out = amp(&dir, &["models", "list", "--available", "--json"], &[]).await;
    let providers = out.json()["providers"].clone();
    let or = providers
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == "openrouter")
        .unwrap();
    assert!(or["models"].to_string().contains("openai/gpt-oss-120b"));
}

#[tokio::test]
async fn the_repo_file_overrides_the_user_file_and_holds_only_roles_and_profile() {
    let dir = workdir("models-repo", Some(7));
    let user = user_file(&dir);
    let repo = repo_file(&dir);

    let out = amp(
        &dir,
        &[
            "models",
            "set",
            "manager",
            "anthropic/claude-sonnet-5-5",
            "--repo",
            "--json",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    assert_eq!(out.json()["scope"], "repo");
    let text = read(&repo);
    assert!(
        text.starts_with("# amp-harness settings for everyone"),
        "{text}"
    );
    assert!(!user.exists(), "--repo leaves the user file alone");
    let r = resolved(&dir, "manager", &[]).await;
    assert_eq!(r["model"], "claude-sonnet-5-5");
    assert!(r["source"].as_str().unwrap().starts_with("repo "), "{r}");

    // A user setting for the same role is written but the repo still wins: a warning says so.
    let out = amp(
        &dir,
        &[
            "models",
            "set",
            "manager",
            "anthropic/claude-opus-5-5",
            "--json",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let o = out.json();
    assert!(
        o["warnings"]
            .to_string()
            .contains("wins over your user config"),
        "{o}"
    );
    assert_eq!(
        resolved(&dir, "manager", &[]).await["model"],
        "claude-sonnet-5-5"
    );
    amp(&dir, &["models", "reset", "manager", "--repo"], &[]).await;
    assert_eq!(
        resolved(&dir, "manager", &[]).await["model"],
        "claude-opus-5-5"
    );

    // A repo file can't define models, inherit or thinking for a profile.
    let before = read(&repo);
    let out = amp(
        &dir,
        &[
            "models",
            "set",
            "manager",
            "openrouter/openai/gpt-oss-120b",
            "--repo",
            "--no-check",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stderr);
    assert!(out.stderr.contains("can't define models"), "{}", out.stderr);
    let out = amp(
        &dir,
        &[
            "models",
            "profile",
            "inherit",
            "--inherit",
            "anthropic/claude-sonnet-5-5",
            "--repo",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert_eq!(read(&repo), before);

    // Repo profile beats the user's.
    let out = amp(&dir, &["models", "profile", "anthropic", "--repo"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let out = amp(&dir, &["models", "profile", "opencode", "--json"], &[]).await;
    assert!(
        out.json()["warnings"]
            .to_string()
            .contains("wins in this repo")
    );
    assert_eq!(
        resolved(&dir, "worker-backend", &[]).await["provider"],
        "anthropic"
    );

    // Outside a project --repo is a usage error.
    let lone = workdir("models-norepo", None);
    let out = amp(&lone, &["models", "reset", "manager", "--repo"], &[]).await;
    assert_eq!(out.code, 2);
    assert!(
        out.stderr.contains("not in an AMP project"),
        "{}",
        out.stderr
    );
}

#[tokio::test]
async fn profile_switches_every_role() {
    let dir = workdir("models-profile", None);
    let user = user_file(&dir);
    let out = amp(&dir, &["models", "profile", "anthropic"], &[]).await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    assert!(out.stdout.contains("profile anthropic"), "{}", out.stdout);
    let report = amp(&dir, &["config", "show", "--json"], &[]).await.json();
    for r in report["roles"].as_array().unwrap() {
        assert_eq!(r["provider"], "anthropic", "{r}");
    }

    let out = amp(
        &dir,
        &[
            "models",
            "profile",
            "inherit",
            "--inherit",
            "anthropic/claude-sonnet-5-5",
            "--thinking",
            "low",
            "--json",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = amp(&dir, &["config", "show", "--json"], &[]).await.json();
    assert_eq!(report["mode"], "inherit");
    for r in report["roles"].as_array().unwrap() {
        assert_eq!(r["model"], "claude-sonnet-5-5", "{r}");
        assert_eq!(r["thinking"], "low", "{r}");
    }

    let before = read(&user);
    let out = amp(&dir, &["models", "profile", "nope"], &[]).await;
    assert_eq!(out.code, 1);
    assert!(
        out.stderr.contains("unknown profile `nope`"),
        "{}",
        out.stderr
    );
    let out = amp(
        &dir,
        &[
            "models",
            "profile",
            "anthropic",
            "--inherit",
            "anthropic/claude-sonnet-5-5",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 2, "{}", out.stderr);
    assert_eq!(read(&user), before);

    // AMP_HARNESS_PROFILE overrides the file: a warning says so.
    let out = amp(
        &dir,
        &["models", "profile", "opencode", "--json"],
        &[("AMP_HARNESS_PROFILE", "anthropic")],
    )
    .await;
    assert!(
        out.json()["warnings"]
            .to_string()
            .contains("AMP_HARNESS_PROFILE")
    );
}

#[tokio::test]
async fn set_checks_the_live_catalog_and_never_prints_the_key() {
    const KEY: &str = "sk-mock-NEVER-PRINT";
    let provider = spawn_provider(ProviderState {
        key: Some(KEY.into()),
        models: vec!["m-1".into(), "m-2".into()],
        ..ProviderState::default()
    })
    .await;
    let dir = workdir("models-live", None);
    let user = user_file(&dir);
    write(
        &user,
        &format!(
            "[providers.mock]\napi = \"openai-completions\"\nbase_url = \"{}\"\nkey_env = \"MOCK_KEY\"\n[providers.mock.models.\"m-1\"]\n",
            provider.url
        ),
    );
    let env = [("MOCK_KEY", KEY)];

    let out = amp(
        &dir,
        &["models", "set", "worker-docs", "mock/m-2", "--json"],
        &env,
    )
    .await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    let o = out.json();
    assert_eq!(o["added_models"][0], "providers.mock.models.\"m-2\"");
    assert_eq!(o["warnings"].as_array().unwrap().len(), 0, "{o}");
    assert_eq!(resolved(&dir, "worker-docs", &env).await["model"], "m-2");

    let before = read(&user);
    let out = amp(&dir, &["models", "set", "worker-docs", "mock/m-9"], &env).await;
    assert_eq!(out.code, 1);
    assert!(
        out.stderr.contains("live catalog has no model `m-9`"),
        "{}",
        out.stderr
    );
    assert_eq!(read(&user), before);
    // --no-check sets it anyway (with a note about defaults).
    let out = amp(
        &dir,
        &["models", "set", "worker-docs", "mock/m-9", "--no-check"],
        &env,
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);

    let out = amp(
        &dir,
        &["models", "list", "--available", "--check", "--json"],
        &env,
    )
    .await;
    let mock = out.json()["providers"]
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == "mock")
        .cloned()
        .unwrap();
    assert_eq!(mock["key_source"], "env MOCK_KEY");

    // The catalog check of `config check --live` flags a model it doesn't list.
    let out = amp(&dir, &["config", "check", "--live", "--json"], &env).await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    let report = out.json();
    let p = report["problems"]
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["area"] == "catalog" && p["key"] == "roles.worker-docs.model")
        .cloned()
        .unwrap_or_else(|| panic!("{report}"));
    assert_eq!(p["layer"], "user");

    assert!(!out.stdout.contains("NEVER-PRINT") && !out.stderr.contains("NEVER-PRINT"));
    let leaked = common::provider::files_containing(&dir, "NEVER-PRINT");
    assert!(leaked.is_empty(), "the key was written to {leaked:?}");
}

#[tokio::test]
async fn config_check_reports_each_layer() {
    let dir = workdir("config-check", Some(7));
    let out = amp(&dir, &["config", "check"], &[]).await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    assert!(
        out.stdout.contains("Checked:") && out.stdout.contains("tui"),
        "{}",
        out.stdout
    );
    // The default profile uses OpenRouter, which has no key here: a warning, not an error.
    assert!(out.stdout.contains("connect openrouter"), "{}", out.stdout);

    write(
        &user_file(&dir),
        "[tui]\ntheme = \"no-such-theme\"\n[roles.reviewer]\nmodel = \"openrouter/no-such\"\n[workers]\nmax_parallel = \"x\"\n[rolse.manager]\nmodel = \"a/b\"\n",
    );
    write(&repo_file(&dir), "[roles.manager]\nmodel = \"nowhere/x\"\n");
    let out = amp(&dir, &["config", "check", "--json"], &[]).await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    let report = out.json();
    let problems = report["problems"].as_array().unwrap();
    let find = |area: &str, key: &str| {
        problems
            .iter()
            .find(|p| p["area"] == area && p["key"] == key)
            .cloned()
            .unwrap_or_else(|| panic!("{area} {key} in {report}"))
    };
    assert_eq!(find("tui", "tui")["layer"], "user");
    assert_eq!(find("workers", "workers")["severity"], "error");
    assert_eq!(find("files", "rolse")["severity"], "warning");
    let reviewer = find("roles", "roles.reviewer");
    assert_eq!(reviewer["layer"], "user");
    assert!(
        reviewer["file"]
            .as_str()
            .unwrap()
            .ends_with("amp-harness/config.toml")
    );
    let manager = find("roles", "roles.manager");
    assert_eq!(manager["layer"], "repo");
    assert!(
        manager["file"]
            .as_str()
            .unwrap()
            .ends_with(".amp/harness/config.toml")
    );
    assert!(manager["fix"].as_str().unwrap().contains("models"));

    // The human output names layer, key and file, and a fix.
    let out = amp(&dir, &["config", "check"], &[]).await;
    assert_eq!(out.code, 1);
    assert!(
        out.stdout.contains("ERROR [repo roles] roles.manager in "),
        "{}",
        out.stdout
    );
    assert!(out.stdout.contains("fix: "), "{}", out.stdout);

    // A repo file with a forbidden table, and a --config file that doesn't exist.
    write(
        &repo_file(&dir),
        "[server]\nurl = \"https://evil.test/mcp\"\n",
    );
    let report = amp(&dir, &["config", "check", "--json"], &[]).await.json();
    assert!(
        report["problems"]
            .as_array()
            .unwrap()
            .iter()
            .any(|p| p["layer"] == "repo"
                && p["area"] == "files"
                && p["message"]
                    .as_str()
                    .unwrap()
                    .contains("`server` is not allowed")),
        "{report}"
    );
    let missing = dir.join("nope.toml");
    let out = amp(
        &dir,
        &[
            "config",
            "check",
            "--json",
            "--config",
            missing.to_str().unwrap(),
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 1);
    assert!(out.stdout.contains("does not exist"), "{}", out.stdout);
}
