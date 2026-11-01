//! The roster as sessions load it, for the commands that report on it (#2335, #2338).
//!
//! A repo agent or skill is used only when it is trusted as it is now (or `--trust-repo`):
//! sessions load through [`Gate::from_env`]. doctor, `agents list/show`, `skills list/show` and
//! `models list` / `config show` load through the same gate here, so a listing never presents an
//! untrusted repo item as in use. The ones left out are listed as "not trusted, not used", with
//! how to trust them.
//!
//! [`Assets::load`] (no gate) is for inspecting and editing the files (`agents new`, `validate`,
//! ...), never for saying what is in use.

use amp_core::{
    assets::{Assets, OverrideDirs},
    repo_trust::{self, Gate, ItemKind, UntrustedItem},
};
use amp_mcp::ConfigPaths;
use serde::Serialize;

/// The trust decision sessions make for `paths`, from the process environment (`--trust-repo`
/// included).
pub fn gate(paths: &ConfigPaths) -> Gate {
    Gate::from_env(paths, &|k| std::env::var(k).ok())
}

/// Built-in, user and repo assets, the repo's only when `gate` lets them through.
pub fn load_assets(paths: &ConfigPaths, gate: &Gate) -> Result<Assets, String> {
    Assets::load_gated(&OverrideDirs::from_paths(paths), gate).map_err(|e| e.to_string())
}

/// The repo's agents and skills `gate` leaves out.
pub fn untrusted(paths: &ConfigPaths, gate: &Gate) -> Vec<UntrustedItem> {
    repo_trust::untrusted(&repo_trust::scan(paths), gate)
        .into_iter()
        .filter(|u| matches!(u.item.kind, ItemKind::Agent | ItemKind::Skill))
        .collect()
}

/// A repo agent or skill that is not used until it is trusted (`--json`: `untrusted`).
#[derive(Debug, Clone, Serialize)]
pub struct UntrustedRow {
    pub name: String,
    pub file: String,
    /// It was trusted before, at another fingerprint.
    pub changed: bool,
}

impl UntrustedRow {
    pub fn from_item(u: &UntrustedItem) -> Self {
        Self {
            name: u.item.name.clone(),
            file: u.item.file.display().to_string(),
            changed: u.changed,
        }
    }
}

/// The rows of `items` of `kind`.
pub fn rows(items: &[UntrustedItem], kind: ItemKind) -> Vec<UntrustedRow> {
    items
        .iter()
        .filter(|u| u.item.kind == kind)
        .map(UntrustedRow::from_item)
        .collect()
}

/// `a (repo), b (repo, changed since you trusted it)`.
pub fn names(rows: &[UntrustedRow]) -> String {
    rows.iter()
        .map(|r| {
            if r.changed {
                format!("{} (repo, changed since you trusted it)", r.name)
            } else {
                format!("{} (repo)", r.name)
            }
        })
        .collect::<Vec<_>>()
        .join(", ")
}

/// How to use them: `--trust-repo` first (#2335), then `amp-harness trust kind:name ...`.
pub fn fix(kind: ItemKind, rows: &[UntrustedRow]) -> Option<String> {
    if rows.is_empty() {
        return None;
    }
    let ids: Vec<String> = rows
        .iter()
        .map(|r| format!("{}:{}", kind.word(), r.name))
        .collect();
    Some(format!(
        "review it, then run with `--trust-repo` to use what the repo brings for one run, or \
         `amp-harness trust {}` to trust it as it is now",
        ids.join(" ")
    ))
}

/// The lines a listing prints under its table when repo items are left out:
///
/// ```text
/// not trusted, not used: repo-helper (repo)
///   review it, then run with `--trust-repo` ..., or `amp-harness trust agent:repo-helper` ...
/// ```
pub fn footer(kind: ItemKind, rows: &[UntrustedRow]) -> Option<String> {
    let fix = fix(kind, rows)?;
    Some(format!("not trusted, not used: {}\n  {fix}", names(rows)))
}

/// The untrusted item of `kind` named `name` (an agent by its canonical id), if any.
pub fn find<'a>(
    items: &'a [UntrustedItem],
    kind: ItemKind,
    name: &str,
) -> Option<&'a UntrustedItem> {
    items.iter().find(|u| {
        u.item.kind == kind
            && match kind {
                ItemKind::Agent => amp_core::policy::canonical_agent_id(&u.item.name) == name,
                _ => u.item.name == name,
            }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn footer_names_every_item_and_leads_with_trust_repo() {
        let rows = vec![
            UntrustedRow {
                name: "a".into(),
                file: "f".into(),
                changed: false,
            },
            UntrustedRow {
                name: "b".into(),
                file: "g".into(),
                changed: true,
            },
        ];
        let text = footer(ItemKind::Agent, &rows).unwrap();
        assert!(
            text.starts_with(
                "not trusted, not used: a (repo), b (repo, changed since you trusted it)\n  review it, then run with `--trust-repo`"
            ),
            "{text}"
        );
        assert!(
            text.contains("`amp-harness trust agent:a agent:b`"),
            "{text}"
        );
        assert!(footer(ItemKind::Skill, &[]).is_none());
    }
}
