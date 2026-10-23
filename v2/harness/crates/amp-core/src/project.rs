//! `.amp.json`: the project marker (D4).
//!
//! The file names the AMP project a checkout belongs to. The harness reads `project_id` (and the
//! name/code for display). The `amp_api` field is never used to pick a server: the URL comes from
//! `--url`, `AMP_MCP_URL`, `config.toml` or the default (amp-mcp's `Settings`). It is read only to
//! tell when the marker was made for another server (#2206, [`ProjectMarker::server_warning`]):
//! project ids are per server, so a parent folder's `.amp.json` made for prod names some other
//! project on a local compose server.

use std::{
    fmt,
    path::{Path, PathBuf},
};

use serde::Deserialize;

/// The marker's file name.
pub const MARKER_FILE: &str = ".amp.json";

/// Stop the upward search at this directory (inclusive), like git's `GIT_CEILING_DIRECTORIES`.
/// Tests set it so a checkout's own `.amp.json` above the test directory is not picked up.
pub const ENV_CEILING_DIR: &str = "AMP_HARNESS_CEILING_DIR";

/// A parsed `.amp.json`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProjectMarker {
    /// Where the file was found.
    pub path: PathBuf,
    pub project_id: i64,
    pub project_name: Option<String>,
    pub project_code: Option<String>,
    /// The AMP origin the marker was made for (`amp_api`), when it names a real one. The old
    /// amp-init skills' placeholder (`http://localhost:8000`) counts as unknown.
    pub amp_api: Option<String>,
}

/// What older amp-init skills wrote as `amp_api` whatever the server was: it says nothing.
const PLACEHOLDER_AMP_API: &str = "http://localhost:8000";

/// Why a `.amp.json` could not be used.
#[derive(Debug)]
pub struct MarkerError {
    pub path: PathBuf,
    pub reason: String,
}

impl fmt::Display for MarkerError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.path.display(), self.reason)
    }
}

impl std::error::Error for MarkerError {}

/// Only the fields the harness uses. Unknown fields (anything added later) are ignored.
#[derive(Deserialize)]
struct RawMarker {
    project_id: Option<serde_json::Value>,
    project_name: Option<String>,
    project_code: Option<String>,
    // Any JSON: a marker with an odd `amp_api` still names its project.
    amp_api: Option<serde_json::Value>,
}

impl ProjectMarker {
    /// Parses the marker text. `path` is only used for messages.
    pub fn parse(path: &Path, text: &str) -> Result<Self, MarkerError> {
        let err = |reason: String| MarkerError {
            path: path.to_path_buf(),
            reason,
        };
        let raw: RawMarker =
            serde_json::from_str(text).map_err(|e| err(format!("not valid JSON: {e}")))?;
        // Older tooling wrote the id as a string; accept both.
        let project_id = match raw.project_id {
            Some(serde_json::Value::Number(n)) => n.as_i64(),
            Some(serde_json::Value::String(s)) => s.trim().parse().ok(),
            _ => None,
        }
        .filter(|id| *id > 0)
        .ok_or_else(|| err("no positive integer project_id".to_owned()))?;
        let non_empty = |s: Option<String>| s.filter(|s| !s.trim().is_empty());
        let amp_api = raw
            .amp_api
            .as_ref()
            .and_then(serde_json::Value::as_str)
            .map(|s| s.trim().to_owned())
            .filter(|s| origin(s).is_some_and(|o| o != PLACEHOLDER_AMP_API));
        Ok(Self {
            path: path.to_path_buf(),
            project_id,
            project_name: non_empty(raw.project_name),
            project_code: non_empty(raw.project_code),
            amp_api,
        })
    }

    /// The marker's `amp_api` when it names another server than `mcp_url` (compared by origin;
    /// `localhost`, `127.0.0.1` and `[::1]` are one host). `None` for a same-origin marker or one
    /// without a usable `amp_api`: those are used as before.
    pub fn other_server(&self, mcp_url: &str) -> Option<&str> {
        let made_for = self.amp_api.as_deref()?;
        (origin(made_for)? != origin(mcp_url)?).then_some(made_for)
    }

    /// Whether the marker's project name disagrees with the name `server_name` this server has for
    /// the same id (case and surrounding space ignored). False when either is unknown.
    pub fn name_differs(&self, server_name: Option<&str>) -> bool {
        match (self.project_name.as_deref(), server_name) {
            (Some(ours), Some(theirs)) => !ours.trim().eq_ignore_ascii_case(theirs.trim()),
            _ => false,
        }
    }

    /// The warning for a marker that may name the wrong project on the server at `mcp_url`
    /// (#2206): made for another server, or its project name differs from `server_name` (this
    /// server's name for `project_id`, `None` when unknown). `None` when nothing disagrees.
    pub fn server_warning(&self, mcp_url: &str, server_name: Option<&str>) -> Option<String> {
        let path = self.path.display();
        let id = self.project_id;
        let theirs = match server_name {
            Some(name) => format!("this server's #{id} is \"{name}\""),
            None => format!(
                "#{id} on this server ({}) may be another project",
                origin(mcp_url).unwrap_or_else(|| mcp_url.to_owned())
            ),
        };
        if let Some(made_for) = self.other_server(mcp_url) {
            return Some(format!(
                ".amp.json at {path} was made for {made_for}; {theirs}"
            ));
        }
        if self.name_differs(server_name) {
            return Some(format!(
                ".amp.json at {path} names project #{id} \"{}\", but {theirs}",
                self.project_name.as_deref().unwrap_or_default()
            ));
        }
        None
    }

    /// Reads one file.
    pub fn read(path: &Path) -> Result<Self, MarkerError> {
        let text = std::fs::read_to_string(path).map_err(|e| MarkerError {
            path: path.to_path_buf(),
            reason: e.to_string(),
        })?;
        Self::parse(path, &text)
    }

    /// Finds the nearest `.amp.json` in `start` or any parent directory. `Ok(None)` when there is
    /// none; a marker that exists but does not parse is an error, not skipped.
    pub fn find(start: &Path) -> Result<Option<Self>, MarkerError> {
        Self::find_within(start, None)
    }

    /// [`Self::find`], searching no higher than `ceiling` (inclusive).
    pub fn find_within(start: &Path, ceiling: Option<&Path>) -> Result<Option<Self>, MarkerError> {
        for dir in start.ancestors() {
            let path = dir.join(MARKER_FILE);
            if path.is_file() {
                return Self::read(&path).map(Some);
            }
            if ceiling.is_some_and(|c| c == dir) {
                break;
            }
        }
        Ok(None)
    }

    /// [`Self::find`] from the current directory, honouring `AMP_HARNESS_CEILING_DIR`.
    pub fn find_from_cwd() -> Result<Option<Self>, MarkerError> {
        let ceiling = std::env::var_os(ENV_CEILING_DIR)
            .filter(|v| !v.is_empty())
            .map(std::path::PathBuf::from);
        match std::env::current_dir() {
            Ok(cwd) => Self::find_within(&cwd, ceiling.as_deref()),
            Err(_) => Ok(None),
        }
    }
}

/// `scheme://host[:port]` of `url`, lower-cased, without a default port, with loopback names
/// written as `localhost`. `None` when `url` has no scheme or host.
pub fn origin(url: &str) -> Option<String> {
    let (scheme, rest) = url.trim().split_once("://")?;
    let scheme = scheme.to_ascii_lowercase();
    let authority = rest.split(['/', '?', '#']).next().unwrap_or(rest);
    // Drop any user:password@.
    let authority = authority.rsplit('@').next().unwrap_or(authority);
    let authority = authority.to_ascii_lowercase();
    let (host, port) = match authority.rsplit_once(':') {
        // `[::1]` alone has colons inside the brackets.
        Some((h, p)) if !p.contains(']') => (h.to_owned(), Some(p.to_owned())),
        _ => (authority.clone(), None),
    };
    let host = host.trim_end_matches('.');
    if host.is_empty() {
        return None;
    }
    let host = match host {
        "127.0.0.1" | "[::1]" => "localhost",
        h => h,
    };
    let default_port = match scheme.as_str() {
        "http" => Some("80"),
        "https" => Some("443"),
        _ => None,
    };
    Some(
        match port.filter(|p| !p.is_empty() && Some(p.as_str()) != default_port) {
            Some(p) => format!("{scheme}://{host}:{p}"),
            None => format!("{scheme}://{host}"),
        },
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p() -> &'static Path {
        Path::new("/x/.amp.json")
    }

    #[test]
    fn reads_the_fields_it_needs_and_ignores_amp_api() {
        let m = ProjectMarker::parse(
            p(),
            r#"{"project_id": 2, "project_name": "AMP v2", "project_code": "amp-v2",
                "amp_api": "https://stale.example", "future_field": [1]}"#,
        )
        .unwrap();
        assert_eq!(m.project_id, 2);
        assert_eq!(m.project_name.as_deref(), Some("AMP v2"));
        assert_eq!(m.project_code.as_deref(), Some("amp-v2"));
        assert_eq!(m.amp_api.as_deref(), Some("https://stale.example"));
    }

    #[test]
    fn origins_compare_without_path_case_default_port_or_loopback_spelling() {
        for (a, b) in [
            (
                "https://amp.simstech.cloud",
                "https://AMP.simstech.cloud/mcp",
            ),
            (
                "https://amp.simstech.cloud:443/",
                "https://amp.simstech.cloud/mcp",
            ),
            ("http://127.0.0.1:18000", "http://localhost:18000/mcp"),
            ("http://[::1]:18000/x", "http://localhost:18000"),
        ] {
            assert_eq!(origin(a), origin(b), "{a} vs {b}");
        }
        for (a, b) in [
            ("https://amp.simstech.cloud", "http://localhost:18000/mcp"),
            ("http://localhost:18000", "http://localhost:3001"),
            ("http://amp.example", "https://amp.example"),
        ] {
            assert_ne!(origin(a), origin(b), "{a} vs {b}");
        }
        assert_eq!(origin("not a url"), None);
        assert_eq!(origin("http:///mcp"), None);
    }

    #[test]
    fn warns_for_a_marker_made_for_another_server_or_naming_another_project() {
        let m = ProjectMarker::parse(
            p(),
            r#"{"project_id": 2, "project_name": "AMP v2", "amp_api": "https://amp.simstech.cloud"}"#,
        )
        .unwrap();
        let compose = "http://localhost:18000/mcp";
        assert_eq!(m.other_server(compose), Some("https://amp.simstech.cloud"));
        assert_eq!(
            m.server_warning(compose, Some("harness-smoke")).unwrap(),
            ".amp.json at /x/.amp.json was made for https://amp.simstech.cloud; this server's #2 \
             is \"harness-smoke\""
        );
        assert_eq!(
            m.server_warning(compose, None).unwrap(),
            ".amp.json at /x/.amp.json was made for https://amp.simstech.cloud; #2 on this server \
             (http://localhost:18000) may be another project"
        );
        // Same origin: used as before; a different name still warns, the same name does not.
        let prod = "https://amp.simstech.cloud/mcp";
        assert_eq!(m.other_server(prod), None);
        assert_eq!(m.server_warning(prod, Some("amp v2 ")), None);
        assert_eq!(m.server_warning(prod, None), None);
        assert_eq!(
            m.server_warning(prod, Some("Other")).unwrap(),
            ".amp.json at /x/.amp.json names project #2 \"AMP v2\", but this server's #2 is \
             \"Other\""
        );
        // No amp_api, the old amp-init placeholder, or junk: nothing to compare.
        for text in [
            r#"{"project_id": 2}"#,
            r#"{"project_id": 2, "amp_api": "http://localhost:8000"}"#,
            r#"{"project_id": 2, "amp_api": "http://localhost:8000/"}"#,
            r#"{"project_id": 2, "amp_api": 7}"#,
            r#"{"project_id": 2, "amp_api": "nonsense"}"#,
        ] {
            let m = ProjectMarker::parse(p(), text).unwrap();
            assert_eq!(m.amp_api, None, "{text}");
            assert_eq!(m.server_warning(compose, Some("x")), None, "{text}");
        }
    }

    #[test]
    fn accepts_a_string_id() {
        let m = ProjectMarker::parse(p(), r#"{"project_id": "17"}"#).unwrap();
        assert_eq!(m.project_id, 17);
        assert_eq!(m.project_name, None);
    }

    #[test]
    fn rejects_missing_or_bad_ids() {
        for text in [
            "{}",
            r#"{"project_id": 0}"#,
            r#"{"project_id": "x"}"#,
            r#"{"project_id": null}"#,
            "not json",
        ] {
            let e = ProjectMarker::parse(p(), text).unwrap_err();
            assert!(e.to_string().starts_with("/x/.amp.json: "), "{text}: {e}");
        }
    }

    #[test]
    fn finds_the_nearest_marker_upwards() {
        // Under cargo's target dir, never /tmp (D13).
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-scratch/marker");
        let _ = std::fs::remove_dir_all(&root);
        let deep = root.join("a/b/c");
        std::fs::create_dir_all(&deep).unwrap();
        std::fs::write(root.join(MARKER_FILE), r#"{"project_id": 5}"#).unwrap();
        std::fs::write(root.join("a").join(MARKER_FILE), r#"{"project_id": 6}"#).unwrap();

        let m = ProjectMarker::find(&deep).unwrap().unwrap();
        assert_eq!(m.project_id, 6);
        assert_eq!(m.path, root.join("a").join(MARKER_FILE));

        // The ceiling stops the search below the markers.
        assert_eq!(
            ProjectMarker::find_within(&deep, Some(&root.join("a/b"))).unwrap(),
            None
        );
        assert_eq!(
            ProjectMarker::find_within(&deep, Some(&root.join("a")))
                .unwrap()
                .unwrap()
                .project_id,
            6
        );

        std::fs::write(root.join("a").join(MARKER_FILE), "{").unwrap();
        assert!(ProjectMarker::find(&deep).is_err());
        let _ = std::fs::remove_dir_all(&root);
    }
}
