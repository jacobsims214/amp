//! doctor's model-side checks (D9 as changed by #2091; D3 for keys):
//!
//! - `roles`: models.toml + config.toml load, and every role resolves to a provider/model.
//! - `key:<provider>`: each provider some role uses has its key (env, then keychain) or needs none.
//!   A missing key fails for the manager's provider (the TUI cannot start) and warns otherwise
//!   (only the roles that use it cannot run).
//! - `models:<provider>`: every model id the roles pin is in the provider's live catalog (the same
//!   lookup as `config show --check`). An unreadable catalog warns; `--offline` skips it.
//!
//! Keys are described by their source only, never by value.

use std::collections::{BTreeMap, BTreeSet};

use amp_core::{
    models::{ModelsConfig, Selection},
    providers::{KeyLookup, KeySource, live_model_ids, resolve_key},
};
use serde::Serialize;

use super::{CheckStatus, Finding};

/// The role the TUI opens as (assets manifest `default_agent`, `amp-manager`).
const PRIMARY_ROLE: &str = "manager";

/// `doctor --json` `.models`. Add fields, never rename or remove them.
#[derive(Debug, Default, Serialize)]
pub struct ModelsReport {
    /// The config.toml read (it may not exist: then the shipped defaults apply).
    pub config_file: Option<String>,
    pub config_found: bool,
    /// `profile` or `inherit`.
    pub mode: Option<&'static str>,
    pub selection: Option<String>,
    /// Why the config could not be loaded.
    pub error: Option<String>,
    /// Roles that do not resolve, with the reason.
    pub role_errors: BTreeMap<String, String>,
    /// The providers some role uses.
    pub providers: Vec<ProviderReport>,
}

#[derive(Debug, Serialize)]
pub struct ProviderReport {
    pub id: String,
    pub api: &'static str,
    pub base_url: String,
    /// `env`, `keychain`, `not-needed` or `missing`.
    pub key: &'static str,
    /// Where the key came from (never the key).
    pub key_source: String,
    pub roles: Vec<String>,
    pub models: Vec<ModelReport>,
    /// `true` when the live catalog was read.
    pub catalog_checked: bool,
    /// Why the live catalog could not be read.
    pub catalog_error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ModelReport {
    pub id: String,
    pub roles: Vec<String>,
    /// `true` listed, `false` missing, `null` not checked.
    pub live: Option<bool>,
}

/// One named finding (`roles`, `key:<id>`, `models:<id>`).
pub type Named = (String, Finding);

pub async fn check(
    config: Result<ModelsConfig, String>,
    config_path: Option<String>,
    keys: &dyn KeyLookup,
    offline: bool,
) -> (ModelsReport, Vec<Named>) {
    let mut report = ModelsReport {
        config_file: config_path.clone(),
        ..ModelsReport::default()
    };
    let mut out = Vec::new();
    let where_ = config_path
        .clone()
        .unwrap_or_else(|| "~/.config/amp-harness/config.toml".to_owned());

    let config = match config {
        Ok(c) => c,
        Err(e) => {
            report.error = Some(e.clone());
            out.push((
                "roles".to_owned(),
                Finding::new(CheckStatus::Fail, e).fix(format!(
                    "fix {where_} (`amp-harness config show` prints the same error)"
                )),
            ));
            return (report, out);
        }
    };
    report.config_file = config
        .config_file
        .as_ref()
        .map(|p| p.display().to_string())
        .or(config_path);
    report.config_found = config.config_found;
    let (mode, selection) = match &config.selection {
        Selection::Profile { name, .. } => ("profile", name.clone()),
        Selection::Inherit { model, .. } => ("inherit", model.clone()),
    };
    report.mode = Some(mode);
    report.selection = Some(selection.clone());

    // provider -> model -> roles
    let mut used: BTreeMap<String, BTreeMap<String, Vec<String>>> = BTreeMap::new();
    let mut primary_provider = None;
    let mut resolved = 0usize;
    for (role, r) in config.resolve_all() {
        match r {
            Ok(r) => {
                resolved += 1;
                if role == PRIMARY_ROLE {
                    primary_provider = Some(r.provider.clone());
                }
                used.entry(r.provider)
                    .or_default()
                    .entry(r.model_id)
                    .or_default()
                    .push(role);
            }
            Err(e) => {
                report.role_errors.insert(role, e.to_string());
            }
        }
    }
    let roles_finding = if report.role_errors.is_empty() {
        Finding::new(
            CheckStatus::Ok,
            format!("{resolved} roles resolve ({mode} {selection})"),
        )
    } else {
        let list: Vec<String> = report
            .role_errors
            .iter()
            .map(|(role, e)| format!("{role}: {e}"))
            .collect();
        Finding::new(CheckStatus::Fail, list.join("; ")).fix(format!(
            "give each role a `provider/model` that exists under [providers] in {where_}, e.g. \
             [roles.{}] model = \"openrouter/<model id>\"",
            report
                .role_errors
                .keys()
                .next()
                .map_or("manager", String::as_str)
        ))
    };
    out.push(("roles".to_owned(), roles_finding));

    for (id, models) in used {
        let Some(provider) = config.providers.get(&id) else {
            continue;
        };
        let mut roles: Vec<String> = models.values().flatten().cloned().collect();
        roles.sort();
        let (_, source) = resolve_key(provider, keys);
        let has_key = !source.is_missing();

        let key_finding = if has_key {
            Finding::new(
                CheckStatus::Ok,
                match &source {
                    KeySource::NotNeeded => "no key needed".to_owned(),
                    other => format!("key from {other}"),
                },
            )
        } else {
            let primary = primary_provider.as_deref() == Some(id.as_str());
            let status = if primary {
                CheckStatus::Fail
            } else {
                CheckStatus::Warn
            };
            Finding::new(
                status,
                format!(
                    "no API key, so these roles cannot run: {}",
                    roles.join(", ")
                ),
            )
            .fix(key_fix(&id, &source, &where_))
        };
        out.push((format!("key:{id}"), key_finding));

        let mut catalog_checked = false;
        let mut catalog_error = None;
        let mut live: BTreeMap<String, Option<bool>> =
            models.keys().map(|m| (m.clone(), None)).collect();
        let models_finding =
            if offline {
                Finding::new(CheckStatus::Skip, "--offline: model catalog not checked")
            } else if !has_key {
                Finding::new(CheckStatus::Skip, "no key: model catalog not checked")
            } else {
                match live_model_ids(provider, keys).await {
                    Ok(ids) => {
                        catalog_checked = true;
                        for (m, v) in live.iter_mut() {
                            *v = Some(ids.contains(m));
                        }
                        models_finding(&id, &models, &ids, &where_)
                    }
                    Err(e) => {
                        catalog_error = Some(e.clone());
                        Finding::new(
                        CheckStatus::Warn,
                        format!("could not read the model catalog at {}: {e}", provider.base_url),
                    )
                    .fix(format!(
                        "check the network and [providers.{id}] base_url, then run `amp-harness \
                         doctor` again (or `--offline` to skip this check)"
                    ))
                    }
                }
            };
        out.push((format!("models:{id}"), models_finding));

        report.providers.push(ProviderReport {
            api: provider.api.as_str(),
            base_url: provider.base_url.clone(),
            key: source.kind(),
            key_source: source.to_string(),
            models: models
                .iter()
                .map(|(m, r)| ModelReport {
                    id: m.clone(),
                    roles: r.clone(),
                    live: live.get(m).copied().flatten(),
                })
                .collect(),
            roles,
            catalog_checked,
            catalog_error,
            id,
        });
    }
    (report, out)
}

fn models_finding(
    provider: &str,
    models: &BTreeMap<String, Vec<String>>,
    ids: &BTreeSet<String>,
    where_: &str,
) -> Finding {
    let missing: Vec<(&String, &Vec<String>)> =
        models.iter().filter(|(m, _)| !ids.contains(*m)).collect();
    if missing.is_empty() {
        let names: Vec<&str> = models.keys().map(String::as_str).collect();
        return Finding::new(
            CheckStatus::Ok,
            format!(
                "{} listed: {}",
                plural(names.len(), "model"),
                names.join(", ")
            ),
        );
    }
    let detail: Vec<String> = missing
        .iter()
        .map(|(m, roles)| format!("{m} (roles: {})", roles.join(", ")))
        .collect();
    let example_role = missing[0].1[0].as_str();
    Finding::new(
        CheckStatus::Fail,
        format!("{provider} does not list {}", detail.join("; ")),
    )
    .fix(format!(
        "pick a model the provider lists and set it in {where_}, e.g. [roles.{example_role}] \
         model = \"{provider}/<model id>\" (`amp-harness config show --check` shows every role)"
    ))
}

fn key_fix(provider: &str, source: &KeySource, where_: &str) -> String {
    match source {
        KeySource::Missing {
            env: Some(var),
            keychain_checked: true,
        } => {
            // #2113: init asks for the key hidden and keeps it in the keychain.
            format!(
                "export {var}=<your {provider} key>, or run `amp-harness init` to store it in the \
                 keychain (asked hidden)"
            )
        }
        KeySource::Missing {
            env: Some(var),
            keychain_checked: false,
        } => format!(
            "export {var}=<your {provider} key> (the keychain is not read while \
             AMP_CREDENTIAL_STORE=file)"
        ),
        _ => format!(
            "add key_env = \"<ENV VAR>\" to [providers.{provider}] in {where_} and export it"
        ),
    }
}

fn plural(n: usize, word: &str) -> String {
    if n == 1 {
        format!("1 {word}")
    } else {
        format!("{n} {word}s")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use amp_core::{models::ModelsFile, providers::StaticKeys};

    fn config(user: &str) -> ModelsConfig {
        let user = ModelsFile::parse(user, "test").unwrap();
        ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config test", &|_| None)
            .unwrap()
    }

    fn find<'a>(out: &'a [Named], name: &str) -> &'a Finding {
        &out.iter().find(|(n, _)| n == name).unwrap().1
    }

    #[tokio::test]
    async fn missing_key_fails_for_the_manager_and_warns_otherwise() {
        // Everything on faux except the reviewers, which use anthropic.
        let c = config(
            "[models]\nprofile = \"inherit\"\ninherit = \"f/m\"\n[providers.f]\napi = \"faux\"\n\
             [providers.f.models.\"m\"]\n[roles.reviewer]\nmodel = \"anthropic/claude-opus-5-5\"\n",
        );
        let keys = StaticKeys::default();
        let (report, out) = check(Ok(c), None, &keys, true).await;
        assert_eq!(find(&out, "roles").status, CheckStatus::Ok);
        assert_eq!(find(&out, "key:f").status, CheckStatus::Ok);
        let anthropic = find(&out, "key:anthropic");
        assert_eq!(anthropic.status, CheckStatus::Warn);
        assert!(anthropic.detail.contains("reviewer"), "{anthropic:?}");
        assert!(
            anthropic
                .fix
                .as_deref()
                .unwrap()
                .starts_with("export ANTHROPIC_API_KEY="),
            "{anthropic:?}"
        );
        assert_eq!(find(&out, "models:anthropic").status, CheckStatus::Skip);
        assert_eq!(report.providers.len(), 2);

        // The manager on anthropic too: now it fails.
        let c = config("[roles.manager]\nmodel = \"anthropic/claude-opus-5-5\"\n");
        let (_, out) = check(Ok(c), None, &keys, true).await;
        assert_eq!(find(&out, "key:anthropic").status, CheckStatus::Fail);
    }

    #[tokio::test]
    async fn keys_are_never_printed() {
        let c = config("[roles.manager]\nmodel = \"anthropic/claude-opus-5-5\"\n");
        let keys = StaticKeys {
            env: BTreeMap::from([
                ("ANTHROPIC_API_KEY".into(), "sk-ant-TOPSECRET".into()),
                ("OPENROUTER_API_KEY".into(), "sk-or-TOPSECRET".into()),
            ]),
            keychain: None,
        };
        let (report, out) = check(Ok(c), None, &keys, true).await;
        let text = format!("{}{out:?}", serde_json::to_string(&report).unwrap());
        assert!(!text.contains("TOPSECRET"), "{text}");
        assert_eq!(
            find(&out, "key:anthropic").detail,
            "key from env ANTHROPIC_API_KEY"
        );
    }

    #[tokio::test]
    async fn config_errors_fail_roles() {
        let (report, out) = check(
            Err("bad toml".into()),
            Some("/c.toml".into()),
            &StaticKeys::default(),
            true,
        )
        .await;
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].1.status, CheckStatus::Fail);
        assert!(out[0].1.fix.as_deref().unwrap().contains("/c.toml"));
        assert_eq!(report.error.as_deref(), Some("bad toml"));
    }

    #[test]
    fn missing_models_name_their_roles() {
        let models = BTreeMap::from([
            ("a".to_owned(), vec!["manager".to_owned()]),
            (
                "b".to_owned(),
                vec!["reviewer".to_owned(), "github-reviewer".to_owned()],
            ),
        ]);
        let ids = BTreeSet::from(["a".to_owned()]);
        let f = models_finding("openrouter", &models, &ids, "/c.toml");
        assert_eq!(f.status, CheckStatus::Fail);
        assert_eq!(
            f.detail,
            "openrouter does not list b (roles: reviewer, github-reviewer)"
        );
        assert!(
            f.fix
                .unwrap()
                .contains("[roles.reviewer] model = \"openrouter/<model id>\"")
        );
        let ids = BTreeSet::from(["a".to_owned(), "b".to_owned()]);
        assert_eq!(
            models_finding("p", &models, &ids, "").status,
            CheckStatus::Ok
        );
    }
}
