//! `amp-harness config show` (M2-3, #2091): per-role provider/model resolution, never a key.

mod common;

use common::{run, workdir};
use serde_json::Value;

fn role<'a>(report: &'a Value, name: &str) -> &'a Value {
    report["roles"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["role"] == name)
        .unwrap_or_else(|| panic!("role {name} in {report}"))
}

#[tokio::test]
async fn shipped_defaults_are_the_opencode_roster() {
    let dir = workdir("config-defaults", None);
    let out = run(
        &dir,
        "http://127.0.0.1:9/mcp",
        &["config", "show", "--json"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = out.json();
    assert_eq!(report["mode"], "profile");
    assert_eq!(report["selection"], "opencode");
    assert_eq!(report["config_found"], false);
    let manager = role(&report, "manager");
    assert_eq!(manager["provider"], "openrouter");
    assert_eq!(manager["model"], "openai/gpt-6.1-sol");
    assert_eq!(manager["ready"], false);
    assert_eq!(role(&report, "worker-backend")["thinking"], "max");
}

#[tokio::test]
async fn two_providers_at_once_and_no_secret_in_output() {
    let dir = workdir("config-two-providers", None);
    let cfg = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&cfg).unwrap();
    std::fs::write(
        cfg.join("config.toml"),
        "[server]\nurl = \"http://localhost:8000/mcp\"\n\n[roles.reviewer]\nmodel = \"anthropic/claude-opus-5-5\"\nthinking = \"xhigh\"\n",
    )
    .unwrap();
    let env = [
        ("ANTHROPIC_API_KEY", "sk-ant-NEVER-PRINT-1"),
        ("OPENROUTER_API_KEY", "sk-or-NEVER-PRINT-2"),
    ];
    for args in [&["config", "show", "--json"][..], &["config", "show"][..]] {
        let out = run(&dir, "http://127.0.0.1:9/mcp", args, &env).await;
        assert_eq!(out.code, 0, "{}", out.stderr);
        assert!(!out.stdout.contains("NEVER-PRINT"), "{}", out.stdout);
        assert!(!out.stderr.contains("NEVER-PRINT"), "{}", out.stderr);
    }
    let report = run(
        &dir,
        "http://127.0.0.1:9/mcp",
        &["config", "show", "--json"],
        &env,
    )
    .await
    .json();
    let reviewer = role(&report, "reviewer");
    assert_eq!(reviewer["provider"], "anthropic");
    assert_eq!(reviewer["model"], "claude-opus-5-5");
    assert_eq!(reviewer["thinking"], "xhigh");
    assert_eq!(reviewer["ready"], true);
    let manager = role(&report, "manager");
    assert_eq!(manager["provider"], "openrouter");
    assert_eq!(manager["ready"], true);
    for p in report["providers"].as_array().unwrap() {
        assert_eq!(p["key"], "env", "{p}");
        assert_eq!(p["in_use"], true, "{p}");
    }
}

#[tokio::test]
async fn profile_env_and_inherit_mode() {
    let dir = workdir("config-inherit", None);
    let out = run(
        &dir,
        "http://127.0.0.1:9/mcp",
        &["config", "show", "--json"],
        &[("AMP_HARNESS_PROFILE", "anthropic")],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stderr);
    let report = out.json();
    assert_eq!(role(&report, "manager")["model"], "claude-opus-5-5");
    assert_eq!(
        role(&report, "worker-backend")["model"],
        "claude-sonnet-5-5"
    );

    let cfg = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&cfg).unwrap();
    std::fs::write(
        cfg.join("config.toml"),
        "[models]\nprofile = \"inherit\"\ninherit = \"openrouter/moonshotai/kimi-k3\"\n",
    )
    .unwrap();
    let report = run(
        &dir,
        "http://127.0.0.1:9/mcp",
        &["config", "show", "--json"],
        &[],
    )
    .await
    .json();
    assert_eq!(report["mode"], "inherit");
    for r in report["roles"].as_array().unwrap() {
        assert_eq!(r["model"], "moonshotai/kimi-k3", "{r}");
    }
}

#[tokio::test]
async fn unresolvable_role_exits_1() {
    let dir = workdir("config-bad-role", None);
    let cfg = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&cfg).unwrap();
    std::fs::write(
        cfg.join("config.toml"),
        "[roles.manager]\nmodel = \"nowhere/x\"\n",
    )
    .unwrap();
    let out = run(&dir, "http://127.0.0.1:9/mcp", &["config", "show"], &[]).await;
    assert_eq!(out.code, 1);
    assert!(
        out.stdout
            .contains("ERROR: role `manager`: unknown provider `nowhere`"),
        "{}",
        out.stdout
    );
}
