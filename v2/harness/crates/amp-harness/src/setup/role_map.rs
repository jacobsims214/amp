//! Which model each role runs on, in one step (#2275): the first run's "your own endpoint" path,
//! `connect --role ROLE=MODEL`, the chat's `/connect … role=model` and an org preset's `[roles]`
//! all end here.
//!
//! The names are the real roles (models.toml / the agent assets) plus four groups, so a person
//! answers four questions instead of eleven:
//!
//! | group | roles |
//! |---|---|
//! | `manager` | manager (plans the work and dispatches it: the orchestrator) |
//! | `workers` (`worker`) | worker-backend, worker-frontend, worker-docs, designer |
//! | `reviewers` (`reviewer` is itself a role) | reviewer, github-reviewer |
//! | `helpers` (`helper`, the small/fast slot) | researcher, media, github-researcher, kb-curator |
//!
//! `all` (or `default`) names every role. An [`Assignment`] is written as `[models] profile =
//! "inherit"` with `inherit` = the default model (every role nobody named, and agents added
//! later), plus `[roles.<role>] model` only for roles on another model. So the roles leave the
//! shipped OpenRouter roster entirely: OpenRouter is no longer in use, asked for or contacted.

use std::collections::BTreeMap;

use amp_core::models::{ModelsConfig, ModelsFile, Selection, normalize_role, parse_model_ref};

use super::{Change, ConfigFile};

/// One group of roles a person maps in one answer.
#[derive(Debug, Clone, Copy)]
pub struct RoleGroup {
    pub name: &'static str,
    /// Other spellings accepted in `--role` and presets.
    pub aliases: &'static [&'static str],
    pub roles: &'static [&'static str],
    /// What the roles do, for the question.
    pub about: &'static str,
    /// Substrings of a model id that suggest it is meant for this group (lowercase).
    hints: &'static [&'static str],
}

pub const GROUPS: &[RoleGroup] = &[
    RoleGroup {
        name: "manager",
        aliases: &[],
        roles: &["manager"],
        about: "plans the work and dispatches it (the orchestrator)",
        hints: &[
            "orchestr", "manager", "planner", "plan", "lead", "opus", "large",
        ],
    },
    RoleGroup {
        name: "workers",
        aliases: &["worker"],
        roles: &[
            "worker-backend",
            "worker-frontend",
            "worker-docs",
            "designer",
        ],
        about: "write the code, docs and designs",
        hints: &[
            "engineer", "coder", "code", "worker", "dev", "sonnet", "medium",
        ],
    },
    RoleGroup {
        name: "reviewers",
        aliases: &[],
        roles: &["reviewer", "github-reviewer"],
        about: "review the work",
        hints: &["review", "critic", "judge"],
    },
    RoleGroup {
        name: "helpers",
        aliases: &["helper"],
        roles: &["researcher", "media", "github-researcher", "kb-curator"],
        about: "small, fast jobs: research, media, KB upkeep",
        hints: &[
            "fast", "small", "mini", "lite", "flash", "haiku", "tiny", "nano", "research",
        ],
    },
];

/// The words that name every role.
const ALL: &[&str] = &["all", "default", "*"];

/// The group `name` names (its own name or an alias).
pub fn group(name: &str) -> Option<&'static RoleGroup> {
    let n = name.trim().to_ascii_lowercase();
    GROUPS
        .iter()
        .find(|g| g.name == n || g.aliases.contains(&n.as_str()))
}

/// Every role the config knows (shipped and the person's), plus every grouped role.
pub fn all_roles(cfg: &ModelsConfig) -> Vec<String> {
    let mut roles = cfg.role_names();
    for g in GROUPS {
        for r in g.roles {
            if !roles.iter().any(|x| x == r) {
                roles.push((*r).to_owned());
            }
        }
    }
    roles.sort();
    roles
}

/// The model each role runs on.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Assignment {
    /// `provider/model`: `[models] inherit`, for every role not in `roles` and agents added later.
    pub default: String,
    /// role -> `provider/model`, for every known role (equal to `default` for most).
    pub roles: BTreeMap<String, String>,
}

impl Assignment {
    /// The model `group` is on (that of its first role).
    pub fn of_group(&self, g: &RoleGroup) -> &str {
        g.roles
            .first()
            .and_then(|r| self.roles.get(*r))
            .map(String::as_str)
            .unwrap_or(&self.default)
    }

    /// Lines for people: `  manager    example/orchestrator` per group, then other roles.
    pub fn lines(&self) -> Vec<String> {
        let mut out = Vec::new();
        for g in GROUPS {
            out.push(format!(
                "  {:<10} {:<40} {}",
                g.name,
                self.of_group(g),
                g.roles.join(", ")
            ));
        }
        let grouped: Vec<&str> = GROUPS
            .iter()
            .flat_map(|g| g.roles.iter().copied())
            .collect();
        for (role, model) in &self.roles {
            if !grouped.contains(&role.as_str()) {
                out.push(format!("  {role:<10} {model}"));
            }
        }
        out
    }
}

/// The model in `models` whose id suggests `g` (first by sorted id), if any.
fn hinted<'a>(g: &RoleGroup, models: &'a [String]) -> Option<&'a String> {
    let mut sorted: Vec<&String> = models.iter().collect();
    sorted.sort();
    sorted.into_iter().find(|m| {
        let lower = m.to_ascii_lowercase();
        g.hints.iter().any(|h| lower.contains(h))
    })
}

/// One model per group, the same way everywhere (first run, `connect --role`, presets): the
/// models `given` for a group win; a group not given takes the model in `models` whose id suggests
/// it (`orchestrator` -> manager, `engineer` -> workers, `review` -> reviewers, `mini` ->
/// helpers); then the followers: **reviewers follow the manager and helpers follow the workers**.
/// The workers fall back to the first model the manager didn't take, and the manager to the
/// workers' model. A group is left out only when nothing at all decides it (no models, nothing
/// given).
pub fn fill_groups(
    given: &BTreeMap<&str, String>,
    models: &[String],
) -> BTreeMap<&'static str, String> {
    let by = |name: &str| GROUPS.iter().find(|g| g.name == name).expect("known group");
    let pick = |name: &str| {
        given
            .get(name)
            .cloned()
            .or_else(|| hinted(by(name), models).cloned())
    };
    let manager_hit = pick("manager");
    let workers = pick("workers")
        .or_else(|| {
            models
                .iter()
                .find(|m| Some(*m) != manager_hit.as_ref())
                .cloned()
        })
        .or_else(|| manager_hit.clone());
    let manager = manager_hit.or_else(|| workers.clone());
    let reviewers = pick("reviewers").or_else(|| manager.clone());
    let helpers = pick("helpers").or_else(|| workers.clone());
    [
        ("manager", manager),
        ("workers", workers),
        ("reviewers", reviewers),
        ("helpers", helpers),
    ]
    .into_iter()
    .filter_map(|(g, m)| m.map(|m| (g, m)))
    .collect()
}

/// Default model per group by name match, for the first run's question: [`fill_groups`] with
/// nothing given. `models` must not be empty.
pub fn guess(models: &[String]) -> BTreeMap<&'static str, String> {
    fill_groups(&BTreeMap::new(), models)
}

/// An assignment from one model per group (`provider/model` refs); every other role gets the
/// workers' model, which is also the default.
pub fn from_groups(per_group: &BTreeMap<&str, String>, roles: &[String]) -> Assignment {
    let default = per_group
        .get("workers")
        .or_else(|| per_group.values().next())
        .cloned()
        .unwrap_or_default();
    let mut out = BTreeMap::new();
    for role in roles {
        let model = GROUPS
            .iter()
            .find(|g| g.roles.contains(&role.as_str()))
            .and_then(|g| per_group.get(g.name))
            .unwrap_or(&default);
        out.insert(role.clone(), model.clone());
    }
    Assignment {
        default,
        roles: out,
    }
}

/// `ROLE=MODEL` pairs (in order; a later pair wins over an earlier one for the same role).
pub fn parse_pairs(pairs: &[String]) -> Result<Vec<(String, String)>, String> {
    pairs
        .iter()
        .map(|p| {
            let (k, v) = p
                .split_once('=')
                .ok_or_else(|| format!("`{p}`: expected ROLE=MODEL (e.g. manager=orchestrator)"))?;
            let (k, v) = (k.trim(), v.trim());
            if k.is_empty() || v.is_empty() {
                return Err(format!(
                    "`{p}`: expected ROLE=MODEL (e.g. manager=orchestrator)"
                ));
            }
            Ok((k.to_owned(), v.to_owned()))
        })
        .collect()
}

/// The roles a `ROLE` word names, or why it names none.
pub fn roles_named(word: &str, roles: &[String]) -> Result<Vec<String>, String> {
    let w = word.trim().to_ascii_lowercase();
    if ALL.contains(&w.as_str()) {
        return Ok(roles.to_vec());
    }
    let role = normalize_role(&w);
    if roles.iter().any(|r| r == role) {
        return Ok(vec![role.to_owned()]);
    }
    if let Some(g) = group(&w) {
        return Ok(g.roles.iter().map(|r| (*r).to_owned()).collect());
    }
    let groups: Vec<&str> = GROUPS.iter().map(|g| g.name).collect();
    Err(format!(
        "unknown role `{word}`: use a group ({}, or all) or a role ({})",
        groups.join(", "),
        roles.join(", ")
    ))
}

/// Resolves `MODEL` to `provider/model`: a bare id is `provider`'s (when given); `p/m` must name a
/// configured provider. `knows(provider, model)` says whether the model exists.
pub fn model_ref(
    model: &str,
    provider: Option<&str>,
    knows: &dyn Fn(&str, &str) -> bool,
    provider_ids: &[String],
) -> Result<String, String> {
    let m = model.trim();
    // `p/m` with a known provider prefix; otherwise the whole text is a model id of `provider`
    // (model ids may contain `/`, e.g. `vendor/model`).
    if let Ok((p, id)) = parse_model_ref(m)
        && provider_ids.iter().any(|x| x == p)
    {
        if knows(p, id) {
            return Ok(format!("{p}/{id}"));
        }
        return Err(format!("{p} has no model `{id}`"));
    }
    let Some(p) = provider else {
        return Err(format!(
            "`{m}`: name the provider too (provider/model; providers: {})",
            provider_ids.join(", ")
        ));
    };
    if knows(p, m) {
        Ok(format!("{p}/{m}"))
    } else {
        Err(format!("{p} has no model `{m}`"))
    }
}

/// Each role's current `provider/model` (roles that don't resolve are left out).
pub fn current(cfg: &ModelsConfig) -> BTreeMap<String, String> {
    cfg.resolve_all()
        .into_iter()
        .filter_map(|(role, r)| r.ok().map(|r| (role, r.model_ref())))
        .collect()
}

/// An assignment from `ROLE=MODEL` pairs on `provider`, whose models are `models`
/// (`provider/model` refs; may be empty). The groups nobody named are filled by [`fill_groups`],
/// the same rule as the first run: so `manager=orchestrator worker=engineer` also puts the
/// reviewers on orchestrator and the helpers on engineer.
///
/// A role nobody named:
/// - takes its group's model when that group follows a group named here (the reviewers follow
///   `manager=`, the helpers follow `workers=`), or when `all` was given;
/// - otherwise keeps its current model when that is already on `provider` (so
///   `--role reviewers=x` alone changes only the reviewers);
/// - otherwise takes its group's model (by name match or following), else the default.
///
/// The default (`[models] inherit`: roles outside the groups, agents added later) is the `all`
/// model, else worker-backend's. Every role ends up on `provider`.
pub fn from_pairs(
    pairs: &[(String, String)],
    roles: &[String],
    resolve_model: &dyn Fn(&str) -> Result<String, String>,
    provider: &str,
    models: &[String],
    current: &BTreeMap<String, String>,
) -> Result<Assignment, String> {
    if pairs.is_empty() {
        return Err("no ROLE=MODEL given".to_owned());
    }
    let mut named: Vec<(Vec<String>, String, String)> = Vec::new();
    for (word, model) in pairs {
        let targets = roles_named(word, roles)?;
        let model = resolve_model(model).map_err(|e| format!("{word}={model}: {e}"))?;
        named.push((targets, model, word.trim().to_ascii_lowercase()));
    }
    let pick = |pred: &dyn Fn(&str) -> bool| {
        named
            .iter()
            .rev()
            .find(|(_, _, w)| pred(w))
            .map(|(_, m, _)| m.clone())
    };
    let all = pick(&|w| ALL.contains(&w));
    // The groups named here (`all` names them all; a later group word wins).
    let mut given: BTreeMap<&str, String> = BTreeMap::new();
    for g in GROUPS {
        let own = pick(&|w| group(w).is_some_and(|x| x.name == g.name));
        if let Some(m) = own.or_else(|| all.clone()) {
            given.insert(g.name, m);
        }
    }
    let on_provider = |m: &&String| m.starts_with(&format!("{provider}/"));
    let filled = fill_groups(&given, models);
    // A group decided by this call: named, or following a group that was named.
    let decided = |g: &str| {
        given.contains_key(g)
            || match g {
                "reviewers" => {
                    given.contains_key("manager") && hinted(by_name(g), models).is_none()
                }
                "helpers" => given.contains_key("workers") && hinted(by_name(g), models).is_none(),
                _ => false,
            }
    };
    let workers_model = given
        .get("workers")
        .cloned()
        .or_else(|| pick(&|w| normalize_role(w) == "worker-backend"))
        .or_else(|| filled.get("workers").cloned())
        .or_else(|| current.get("worker-backend").filter(on_provider).cloned())
        .unwrap_or_else(|| named[0].1.clone());
    let mut out: BTreeMap<String, String> = BTreeMap::new();
    for r in roles {
        let g = GROUPS.iter().find(|g| g.roles.contains(&r.as_str()));
        let model = match g {
            Some(g) if decided(g.name) => filled[g.name].clone(),
            _ if all.is_some() => all.clone().unwrap_or_default(),
            _ => match current.get(r).filter(on_provider) {
                Some(keep) => keep.clone(),
                None => g
                    .filter(|g| g.name != "workers")
                    .and_then(|g| filled.get(g.name))
                    .cloned()
                    .unwrap_or_else(|| workers_model.clone()),
            },
        };
        out.insert(r.clone(), model);
    }
    for (targets, model, _) in &named {
        for r in targets {
            out.insert(r.clone(), model.clone());
        }
    }
    let default = all.unwrap_or_else(|| {
        out.get("worker-backend")
            .cloned()
            .unwrap_or_else(|| workers_model.clone())
    });
    Ok(Assignment {
        default,
        roles: out,
    })
}

fn by_name(name: &str) -> &'static RoleGroup {
    GROUPS.iter().find(|g| g.name == name).expect("known group")
}

/// The changes that put `a` into config.toml (`doc` is the file's current table): inherit mode
/// with `a.default`, and `[roles.<role>] model` where a role differs from it, or where the file
/// already names a model for it (so an old pin can't win over the new mapping). A model change
/// drops that table's `extra_body` (it was written for the old model).
pub fn changes(a: &Assignment, doc: &toml::Table) -> Vec<Change> {
    let mut out = vec![
        Change::set("models", "profile", amp_core::models::INHERIT),
        Change::set("models", "inherit", &a.default),
    ];
    let roles_table = doc.get("roles").and_then(toml::Value::as_table);
    for (role, model) in &a.roles {
        let (name, existing) = match roles_table.and_then(|t| {
            t.get(role.as_str()).map(|v| (role.clone(), v)).or_else(|| {
                t.get(&format!("amp-{role}"))
                    .map(|v| (format!("amp-{role}"), v))
            })
        }) {
            Some((name, v)) => (name, v.as_table()),
            None => (role.clone(), None),
        };
        let old = existing
            .and_then(|t| t.get("model"))
            .and_then(toml::Value::as_str);
        if *model == a.default && old.is_none() {
            continue;
        }
        if old == Some(model.as_str()) {
            continue;
        }
        let section = format!("roles.{}", super::config_file::toml_key(&name));
        if existing.is_some_and(|t| t.contains_key("extra_body")) {
            out.push(Change::remove(&section, "extra_body"));
            out.push(Change::DropTables(format!("{section}.extra_body")));
        }
        out.push(Change::set(&section, "model", model));
    }
    out
}

/// Writes `a` to `config`. Checked first: with the new text every role must resolve to its model
/// (the user layer over the defaults), or nothing is written. Returns the backup path, if any.
pub fn write(config: &ConfigFile, a: &Assignment) -> Result<Option<std::path::PathBuf>, String> {
    let text = config.text()?.unwrap_or_default();
    let doc: toml::Table = toml::from_str(&text)
        .map_err(|e| format!("{} does not parse: {}", config.path.display(), e.message()))?;
    let changes = changes(a, &doc);
    let preview = config.preview(&changes)?;
    let user = ModelsFile::parse(&preview, "config.toml").map_err(|e| e.0)?;
    let cfg = ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config", &|_| None)
        .map_err(|e| e.0)?;
    if !matches!(cfg.selection, Selection::Inherit { .. }) {
        return Err("the roles did not switch to inherit mode".to_owned());
    }
    for (role, want) in &a.roles {
        let got = cfg.resolve_role(role).map_err(|e| e.0)?;
        if got.model_ref() != *want {
            return Err(format!(
                "`{role}` would run on {} instead of {want}",
                got.model_ref()
            ));
        }
    }
    config.edit(&changes)
}

/// Roles the repo's `.amp/harness/config.toml` keeps on another model than `a` says (it wins
/// over the user file), for a warning.
pub fn overridden(cfg: &ModelsConfig, a: &Assignment) -> Vec<String> {
    a.roles
        .iter()
        .filter_map(|(role, want)| {
            let got = cfg.resolve_role(role).ok()?;
            (got.model_ref() != *want && got.source.contains("repo "))
                .then(|| format!("{role} (repo: {})", got.model_ref()))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn roles() -> Vec<String> {
        let cfg = ModelsConfig::from_files(ModelsFile::defaults(), None, "t", &|_| None).unwrap();
        all_roles(&cfg)
    }

    #[test]
    fn groups_cover_every_shipped_role_once() {
        let shipped = ModelsConfig::from_files(ModelsFile::defaults(), None, "t", &|_| None)
            .unwrap()
            .role_names();
        let mut grouped: Vec<&str> = GROUPS
            .iter()
            .flat_map(|g| g.roles.iter().copied())
            .collect();
        grouped.sort();
        let before = grouped.len();
        grouped.dedup();
        assert_eq!(before, grouped.len(), "a role in two groups");
        assert_eq!(
            grouped,
            shipped.iter().map(String::as_str).collect::<Vec<_>>(),
            "the groups must name exactly the shipped roles (models.toml)"
        );
    }

    #[test]
    fn names_guess_the_groups() {
        let g = guess(&["engineer".into(), "orchestrator".into()]);
        assert_eq!(g["manager"], "orchestrator");
        assert_eq!(g["workers"], "engineer");
        assert_eq!(g["reviewers"], "orchestrator");
        assert_eq!(g["helpers"], "engineer");
        let g = guess(&["big-planner".into(), "coder".into(), "coder-mini".into()]);
        assert_eq!(
            (
                g["manager"].as_str(),
                g["workers"].as_str(),
                g["helpers"].as_str()
            ),
            ("big-planner", "coder", "coder-mini")
        );
        let g = guess(&["only".into()]);
        assert!(g.values().all(|m| m == "only"));
        // No hints at all: the first model for everyone.
        let g = guess(&["a".into(), "b".into()]);
        assert_eq!((g["manager"].as_str(), g["workers"].as_str()), ("a", "a"));
    }

    #[test]
    fn pairs_map_groups_roles_and_a_default() {
        let roles = roles();
        let knows = |p: &str, m: &str| p == "ex" && ["engineer", "orchestrator"].contains(&m);
        let ids = vec!["ex".to_owned(), "openrouter".to_owned()];
        let resolve = |m: &str| model_ref(m, Some("ex"), &knows, &ids);
        let models = vec!["ex/engineer".to_owned(), "ex/orchestrator".to_owned()];
        let pairs =
            parse_pairs(&["manager=orchestrator".into(), "worker=engineer".into()]).unwrap();
        let a = from_pairs(&pairs, &roles, &resolve, "ex", &models, &BTreeMap::new()).unwrap();
        assert_eq!(a.default, "ex/engineer");
        assert_eq!(a.roles["manager"], "ex/orchestrator");
        assert_eq!(a.roles["worker-docs"], "ex/engineer");
        // #2307: the same as the first run: reviewers follow the manager, helpers the workers.
        assert_eq!(a.roles["reviewer"], "ex/orchestrator");
        assert_eq!(a.roles["github-reviewer"], "ex/orchestrator");
        assert_eq!(a.roles["researcher"], "ex/engineer");
        assert!(a.roles.values().all(|m| m.starts_with("ex/")));
        // ... even when the roles are already on this provider (a re-connect).
        let on_ex: BTreeMap<String, String> = roles
            .iter()
            .map(|r| (r.clone(), "ex/engineer".to_owned()))
            .collect();
        let again = from_pairs(&pairs, &roles, &resolve, "ex", &models, &on_ex).unwrap();
        assert_eq!(again, a);

        // A role after its group wins; amp- prefixes are fine; `all` sets the default.
        let pairs = parse_pairs(&[
            "all=orchestrator".into(),
            "workers=engineer".into(),
            "amp-worker-docs=ex/orchestrator".into(),
        ])
        .unwrap();
        let a = from_pairs(&pairs, &roles, &resolve, "ex", &models, &BTreeMap::new()).unwrap();
        assert_eq!(a.default, "ex/orchestrator");
        assert_eq!(a.roles["worker-backend"], "ex/engineer");
        assert_eq!(a.roles["worker-docs"], "ex/orchestrator");

        for (bad, why) in [
            ("boss=engineer", "unknown role `boss`"),
            ("manager=gpt-9", "ex has no model `gpt-9`"),
            ("manager=openrouter/nope", "openrouter has no model `nope`"),
        ] {
            let pairs = parse_pairs(&[bad.into()]).unwrap();
            let e =
                from_pairs(&pairs, &roles, &resolve, "ex", &models, &BTreeMap::new()).unwrap_err();
            assert!(e.contains(why), "{bad}: {e}");
        }
        // Only the reviewers: the rest keep their models on this provider; a role still on
        // another provider moves to the default.
        let mut now: BTreeMap<String, String> = roles
            .iter()
            .map(|r| (r.clone(), "ex/engineer".to_owned()))
            .collect();
        now.insert("manager".into(), "ex/orchestrator".into());
        now.insert("media".into(), "openrouter/x".into());
        let pairs = parse_pairs(&["reviewers=orchestrator".into()]).unwrap();
        let a = from_pairs(&pairs, &roles, &resolve, "ex", &models, &now).unwrap();
        assert_eq!(a.default, "ex/engineer");
        assert_eq!(a.roles["manager"], "ex/orchestrator");
        assert_eq!(a.roles["reviewer"], "ex/orchestrator");
        assert_eq!(a.roles["worker-backend"], "ex/engineer");
        assert_eq!(a.roles["media"], "ex/engineer");
        assert!(parse_pairs(&["manager".into()]).is_err());
        assert!(parse_pairs(&["=x".into()]).is_err());
    }

    #[test]
    fn connect_setup_and_presets_share_one_group_rule() {
        // #2307: the first run's guess is fill_groups with nothing given...
        let models: Vec<String> = ["engineer", "orchestrator", "code-reviewer", "fast-mini"]
            .map(String::from)
            .to_vec();
        assert_eq!(guess(&models), fill_groups(&BTreeMap::new(), &models));
        // ... and what is given wins, the rest is matched by name or follows.
        let given = BTreeMap::from([("manager", "orchestrator".to_owned())]);
        let g = fill_groups(&given, &["engineer".into(), "orchestrator".into()]);
        assert_eq!(g["reviewers"], "orchestrator");
        assert_eq!(g["helpers"], "engineer");
        let g = fill_groups(&given, &models);
        assert_eq!(g["reviewers"], "code-reviewer");
        assert_eq!(g["helpers"], "fast-mini");
        // Nothing to go on: only what is given (and its followers).
        let g = fill_groups(&given, &[]);
        assert_eq!(g.len(), 4, "{g:?}");
        assert!(g.values().all(|m| m == "orchestrator"));
        assert!(fill_groups(&BTreeMap::new(), &[]).is_empty());

        // from_pairs with a reviewer model on the provider: it is matched by name, as at setup.
        let roles = roles();
        let knows = |p: &str, m: &str| {
            p == "ex" && ["engineer", "orchestrator", "code-reviewer"].contains(&m)
        };
        let ids = vec!["ex".to_owned()];
        let resolve = |m: &str| model_ref(m, Some("ex"), &knows, &ids);
        let refs: Vec<String> = ["engineer", "orchestrator", "code-reviewer"]
            .map(|m| format!("ex/{m}"))
            .to_vec();
        let pairs =
            parse_pairs(&["manager=orchestrator".into(), "workers=engineer".into()]).unwrap();
        let a = from_pairs(&pairs, &roles, &resolve, "ex", &refs, &BTreeMap::new()).unwrap();
        assert_eq!(a.roles["reviewer"], "ex/code-reviewer");
        assert_eq!(a.roles["kb-curator"], "ex/engineer");
        // worker-backend alone changes worker-backend only; the others keep theirs.
        let on_ex: BTreeMap<String, String> = roles
            .iter()
            .map(|r| (r.clone(), "ex/orchestrator".to_owned()))
            .collect();
        let pairs = parse_pairs(&["worker-backend=engineer".into()]).unwrap();
        let a = from_pairs(&pairs, &roles, &resolve, "ex", &refs, &on_ex).unwrap();
        assert_eq!(a.roles["worker-backend"], "ex/engineer");
        assert_eq!(a.roles["worker-docs"], "ex/orchestrator");
        assert_eq!(a.roles["researcher"], "ex/orchestrator");
    }

    #[test]
    fn changes_write_inherit_and_only_the_differences() {
        let roles = roles();
        let per_group = BTreeMap::from([
            ("manager", "ex/orchestrator".to_owned()),
            ("workers", "ex/engineer".to_owned()),
            ("reviewers", "ex/orchestrator".to_owned()),
            ("helpers", "ex/engineer".to_owned()),
        ]);
        let a = from_groups(&per_group, &roles);
        assert_eq!(a.default, "ex/engineer");
        let doc: toml::Table = toml::from_str(
            "[roles.researcher]\nmodel = \"openrouter/x\"\n[roles.amp-designer]\nmodel = \"ex/engineer\"\n\
             [roles.manager]\nmodel = \"openrouter/y\"\nthinking = \"high\"\nextra_body = { a = 1 }\n",
        )
        .unwrap();
        let ch = changes(&a, &doc);
        let sets: Vec<String> = ch
            .iter()
            .filter_map(|c| match c {
                Change::Set {
                    section,
                    key,
                    value,
                    ..
                } => Some(format!("{section}.{key}={value}")),
                _ => None,
            })
            .collect();
        assert_eq!(
            sets,
            vec![
                "models.profile=inherit",
                "models.inherit=ex/engineer",
                "roles.github-reviewer.model=ex/orchestrator",
                "roles.manager.model=ex/orchestrator",
                // An old pin to another provider is moved to the default, not left to win.
                "roles.researcher.model=ex/engineer",
                "roles.reviewer.model=ex/orchestrator",
            ]
        );
        assert!(ch.contains(&Change::remove("roles.manager", "extra_body")));
    }
}
