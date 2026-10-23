//! `.amp.json`: the project marker (D4).
//!
//! The file names the AMP project a checkout belongs to. The harness reads `project_id` (and the
//! name/code for display) and ignores every other field. In particular the old `amp_api` field is
//! stale on most checkouts and is never used to pick a server: the URL comes from `--url`,
//! `AMP_MCP_URL`, `config.toml` or the default (amp-mcp's `Settings`).

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
}

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

/// Only the fields the harness uses. Unknown fields (`amp_api`, anything added later) are ignored.
#[derive(Deserialize)]
struct RawMarker {
    project_id: Option<serde_json::Value>,
    project_name: Option<String>,
    project_code: Option<String>,
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
        Ok(Self {
            path: path.to_path_buf(),
            project_id,
            project_name: non_empty(raw.project_name),
            project_code: non_empty(raw.project_code),
        })
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
