//! Turning a resolved `[mcp.<name>]` stdio server into an [`amp_mcp::StdioSpec`] (#2236, H3).
//!
//! The environment rule (D7 of decisions/amp-harness-configurability.md), the same scrub bash gets
//! (#2142):
//! 1. the process environment, minus rpi's host-private plumbing ([`is_host_private_env`]) and
//!    minus every secret name the runtime knows ([`Runtime::secret_env`] plus `pass_env`: AMP_TOKEN,
//!    provider keys, `--token-env`, every MCP server's `${env:..}`/`bearer_env`/`secret_env`);
//! 2. with `env_inherit = "minimal"`, only [`MINIMAL_ENV`] and `LC_*` of what is left;
//! 3. then the server's `env_file` and `env` (already expanded by [`McpServer::resolve`]);
//! 4. then its keychain `secret_env` values, last.
//!
//! amp-mcp clears the child's environment and sets exactly this map, so nothing else leaks in.
//! Secret values are only ever in the returned spec, never in errors or `Debug`.
//!
//! [`Runtime::secret_env`]: crate::agent::Runtime::secret_env

use std::{
    collections::{BTreeMap, BTreeSet},
    path::{Path, PathBuf},
};

use amp_mcp::{ExternalOptions, ProtocolMode, StdioSpec, external::stderr_log_path};
use rpi_tools::is_host_private_env;

use crate::mcp_config::{
    EnvInherit, MINIMAL_ENV, McpProblem, McpServer, ResolvedStdio, SecretRef, SecretTarget,
    Severity,
};

/// What [`stdio_spec`] needs besides the server.
pub struct StdioContext<'a> {
    /// The harness's environment, read now (`std::env::vars()` in production).
    pub process_env: Vec<(String, String)>,
    /// Names never inherited: [`crate::agent::Runtime::secret_env`] plus its `pass_env`.
    pub strip: &'a BTreeSet<String>,
    /// `AMP_CREDENTIAL_STORE=file`: keychain secrets are refused (D2).
    pub file_credential_store: bool,
    /// Reads one secret: [`SecretRef::fetch`] with [`amp_mcp::keychain_service`] in production.
    pub fetch: &'a dyn Fn(&SecretRef) -> Result<Option<String>, String>,
    /// The harness state dir; logs go to `<state>/mcp-logs/`.
    pub state_dir: &'a Path,
    /// Log file name without `.log` (default: the server name). A worktree-scoped instance passes
    /// its own (e.g. `fs-2236`) so instances do not share one file.
    pub log_name: Option<&'a str>,
    /// The working directory when the server sets no `cwd`: the worktree for a worktree-scoped
    /// instance, else the repo root (else the harness's own).
    pub default_cwd: &'a Path,
}

impl<'a> StdioContext<'a> {
    /// The production context: the real environment and keychain.
    pub fn from_process(
        strip: &'a BTreeSet<String>,
        fetch: &'a dyn Fn(&SecretRef) -> Result<Option<String>, String>,
        state_dir: &'a Path,
        default_cwd: &'a Path,
    ) -> Self {
        Self {
            process_env: std::env::vars().collect(),
            strip,
            file_credential_store: std::env::var(amp_mcp::ENV_CREDENTIAL_STORE)
                .is_ok_and(|v| v.trim().eq_ignore_ascii_case("file")),
            fetch,
            state_dir,
            log_name: None,
            default_cwd,
        }
    }
}

/// The names [`StdioContext::strip`] holds: the runtime's secret names plus the ones passed to bash
/// on purpose (`[secrets] pass_to_bash` is for bash only, not for MCP servers).
pub fn strip_set(secret_env: &BTreeSet<String>, pass_env: &BTreeSet<String>) -> BTreeSet<String> {
    secret_env.union(pass_env).cloned().collect()
}

/// Steps 1 and 2 of the rule: what a stdio server inherits from the harness.
pub fn inherited_env(
    process_env: impl IntoIterator<Item = (String, String)>,
    strip: &BTreeSet<String>,
    inherit: EnvInherit,
) -> BTreeMap<String, String> {
    process_env
        .into_iter()
        .filter(|(k, _)| !is_host_private_env(k) && !strip.contains(k))
        .filter(|(k, _)| match inherit {
            EnvInherit::All => true,
            EnvInherit::Minimal => MINIMAL_ENV.contains(&k.as_str()) || k.starts_with("LC_"),
        })
        .collect()
}

fn problem(server: &McpServer, key: &str, message: String, fix: Option<String>) -> McpProblem {
    McpProblem {
        severity: Severity::Error,
        file: server.origin.file.clone(),
        key: format!("mcp.{}.{key}", server.name),
        message,
        fix,
    }
}

/// Builds the spawn spec for `server` from its [`McpServer::resolve`] result: the D7 environment
/// with secrets fetched, the working directory, the stderr log and the timeouts.
pub fn stdio_spec(
    server: &McpServer,
    resolved: ResolvedStdio,
    ctx: &StdioContext<'_>,
) -> Result<StdioSpec, McpProblem> {
    if ctx.file_credential_store && server.uses_keychain() {
        return Err(problem(
            server,
            "secret_env",
            format!(
                "its secrets live in the keychain, but {}=file has no keychain",
                amp_mcp::ENV_CREDENTIAL_STORE
            ),
            Some("pass them as `env = { NAME = \"${env:NAME}\" }` instead".to_owned()),
        ));
    }

    let mut env = inherited_env(
        ctx.process_env.iter().cloned(),
        ctx.strip,
        resolved.env_inherit,
    );
    env.extend(resolved.env);
    for secret in &resolved.secrets {
        let SecretTarget::Env(var) = &secret.target else {
            continue;
        };
        match (ctx.fetch)(secret) {
            Ok(Some(value)) => {
                env.insert(var.clone(), value);
            }
            Ok(None) => {
                return Err(problem(
                    server,
                    "secret_env",
                    format!("`{var}` has no value ({})", secret.describe()),
                    Some(format!(
                        "store it with `amp-harness mcp secret {}` (or remove it from \
                         secret_env){}",
                        server.name,
                        crate::mcp_config::adopt_hint(server)
                    )),
                ));
            }
            Err(e) => {
                return Err(problem(server, "secret_env", format!("`{var}`: {e}"), None));
            }
        }
    }

    let cwd: PathBuf = resolved
        .cwd
        .unwrap_or_else(|| ctx.default_cwd.to_path_buf());
    let log = stderr_log_path(ctx.state_dir, ctx.log_name.unwrap_or(&server.name));
    Ok(StdioSpec {
        name: server.name.clone(),
        command: resolved.command,
        args: resolved.args,
        env,
        cwd,
        stderr_log: log,
        options: ExternalOptions {
            connect_timeout: server.startup_timeout,
            call_timeout: server.call_timeout,
            protocol: ProtocolMode::Legacy,
            ..ExternalOptions::default()
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mcp_config::{ExpandContext, McpConfig, Resolved, SecretSource};

    fn server(toml_text: &str) -> McpServer {
        let file: toml::Table = toml::from_str(toml_text).unwrap();
        let user = file["mcp"].as_table().unwrap().clone();
        let cfg = McpConfig::from_tables(
            Some((&user, Path::new("/home/u/.config/amp-harness/config.toml"))),
            None,
        );
        assert!(cfg.problems.is_empty(), "{:?}", cfg.problems);
        cfg.servers.into_values().next().unwrap()
    }

    fn resolve(s: &McpServer, env: &[(&str, &str)]) -> ResolvedStdio {
        let env: BTreeMap<String, String> = env
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        let lookup = move |k: &str| env.get(k).cloned();
        let ctx = ExpandContext {
            env: &lookup,
            repo: Some(PathBuf::from("/repo")),
            worktree: None,
            home: Some(PathBuf::from("/home/u")),
            config_dir: Some(PathBuf::from("/home/u/.config/amp-harness")),
        };
        match s.resolve(&ctx).unwrap() {
            Resolved::Stdio(r) => r,
            other => panic!("{other:?}"),
        }
    }

    const PROCESS: &[(&str, &str)] = &[
        ("PATH", "/usr/bin"),
        ("HOME", "/home/u"),
        ("LC_ALL", "C"),
        ("EDITOR", "vim"),
        ("AMP_TOKEN", "amp-dummy-token"),
        ("ANTHROPIC_API_KEY", "provider-dummy-key"),
        ("OTHER_SERVER_TOKEN", "other-dummy-token"),
        ("PASSED_TO_BASH", "bash-only-dummy"),
        ("API_URL", "https://api.example"),
    ];

    fn process() -> Vec<(String, String)> {
        PROCESS
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    fn strip() -> BTreeSet<String> {
        let secret: BTreeSet<String> = ["AMP_TOKEN", "ANTHROPIC_API_KEY", "OTHER_SERVER_TOKEN"]
            .map(String::from)
            .into();
        strip_set(&secret, &BTreeSet::from(["PASSED_TO_BASH".to_owned()]))
    }

    const TOML: &str = r#"
        [mcp.fs]
        command = "npx"
        args = ["-y", "server"]
        env = { LOG_LEVEL = "info", API_URL = "${env:API_URL}" }
        secret_env = ["BRAVE_API_KEY"]
        startup_timeout_secs = 7
        call_timeout_secs = 9
    "#;

    #[test]
    fn env_strips_secrets_and_adds_the_servers_own() {
        let s = server(TOML);
        let resolved = resolve(&s, PROCESS);
        let strip = strip();
        let fetched = std::cell::RefCell::new(Vec::new());
        let fetch = |r: &SecretRef| {
            fetched.borrow_mut().push(r.clone());
            Ok(Some("brave-dummy-key".to_owned()))
        };
        let ctx = StdioContext {
            process_env: process(),
            strip: &strip,
            file_credential_store: false,
            fetch: &fetch,
            state_dir: Path::new("/state"),
            log_name: None,
            default_cwd: Path::new("/repo"),
        };
        let spec = stdio_spec(&s, resolved, &ctx).unwrap();
        for gone in [
            "AMP_TOKEN",
            "ANTHROPIC_API_KEY",
            "OTHER_SERVER_TOKEN",
            "PASSED_TO_BASH",
        ] {
            assert!(!spec.env.contains_key(gone), "{gone} leaked");
        }
        let values: String = spec.env.values().cloned().collect::<Vec<_>>().join(" ");
        assert!(!values.contains("dummy-token") && !values.contains("provider-dummy"));
        assert_eq!(spec.env["PATH"], "/usr/bin");
        assert_eq!(spec.env["EDITOR"], "vim");
        assert_eq!(spec.env["LOG_LEVEL"], "info");
        assert_eq!(spec.env["API_URL"], "https://api.example");
        assert_eq!(spec.env["BRAVE_API_KEY"], "brave-dummy-key");
        assert_eq!(
            fetched.borrow()[0].source,
            SecretSource::Keychain {
                account: "mcp:fs:env:BRAVE_API_KEY".into()
            }
        );
        assert_eq!(spec.cwd, PathBuf::from("/repo"));
        assert_eq!(spec.stderr_log, PathBuf::from("/state/mcp-logs/fs.log"));
        assert_eq!(spec.options.connect_timeout.as_secs(), 7);
        assert_eq!(spec.options.call_timeout.as_secs(), 9);
        assert_eq!(spec.command, "npx");
        assert!(!format!("{spec:?}").contains("brave-dummy"));
    }

    #[test]
    fn host_private_vars_are_not_inherited() {
        // Whatever rpi calls host-private stays out, as for bash.
        let probe: Vec<(String, String)> = ["SSH_AUTH_SOCK", "TERM_SESSION_ID", "PATH"]
            .iter()
            .map(|k| ((*k).to_owned(), "x".to_owned()))
            .collect();
        let env = inherited_env(probe.clone(), &BTreeSet::new(), EnvInherit::All);
        for (k, _) in &probe {
            assert_eq!(env.contains_key(k), !is_host_private_env(k), "{k}");
        }
    }

    #[test]
    fn minimal_inherit_keeps_only_the_basics() {
        let env = inherited_env(process(), &strip(), EnvInherit::Minimal);
        let names: Vec<&str> = env.keys().map(String::as_str).collect();
        assert_eq!(names, ["HOME", "LC_ALL", "PATH"]);
    }

    #[test]
    fn server_env_wins_over_inherited_and_secrets_win_last() {
        let s = server(
            r#"
            [mcp.x]
            command = "srv"
            env = { EDITOR = "nano" }
            secret_env = ["EDITOR2"]
            cwd = "/work"
        "#,
        );
        let resolved = resolve(&s, &[]);
        let strip = BTreeSet::new();
        let fetch = |_: &SecretRef| Ok(Some("s".to_owned()));
        let mut ctx = StdioContext {
            process_env: process(),
            strip: &strip,
            file_credential_store: false,
            fetch: &fetch,
            state_dir: Path::new("/state"),
            log_name: Some("x-2236"),
            default_cwd: Path::new("/repo"),
        };
        let spec = stdio_spec(&s, resolved.clone(), &ctx).unwrap();
        assert_eq!(spec.env["EDITOR"], "nano");
        assert_eq!(spec.env["EDITOR2"], "s");
        assert_eq!(spec.cwd, PathBuf::from("/work"));
        assert_eq!(spec.stderr_log, PathBuf::from("/state/mcp-logs/x-2236.log"));

        // A missing keychain value names the variable and account, never a value.
        let none = |_: &SecretRef| Ok(None);
        ctx.fetch = &none;
        let err = stdio_spec(&s, resolved.clone(), &ctx).unwrap_err();
        assert_eq!(err.key, "mcp.x.secret_env");
        assert!(err.message.contains("EDITOR2"), "{err}");
        assert!(err.message.contains("keychain mcp:x:env:EDITOR2"), "{err}");

        // AMP_CREDENTIAL_STORE=file cannot serve keychain secrets.
        ctx.fetch = &fetch;
        ctx.file_credential_store = true;
        let err = stdio_spec(&s, resolved, &ctx).unwrap_err();
        assert!(
            err.to_string().contains("AMP_CREDENTIAL_STORE=file"),
            "{err}"
        );
    }

    #[test]
    fn file_store_is_fine_without_keychain_secrets() {
        let s = server("[mcp.y]\ncommand = \"srv\"\n");
        let resolved = resolve(&s, &[]);
        let strip = BTreeSet::new();
        let fetch = |_: &SecretRef| -> Result<Option<String>, String> { panic!("no secrets") };
        let ctx = StdioContext {
            process_env: vec![],
            strip: &strip,
            file_credential_store: true,
            fetch: &fetch,
            state_dir: Path::new("/state"),
            log_name: None,
            default_cwd: Path::new("/repo"),
        };
        assert!(stdio_spec(&s, resolved, &ctx).is_ok());
    }
}
