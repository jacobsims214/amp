//! Following the chat folder's `.amp.json` while the chat runs (#2383).
//!
//! The manager writes `.amp.json` itself when a person asks it to set up a project (amp-init,
//! #2363). Before this, the chat kept what it found at start, so the header and status line said
//! "no project" for the rest of the session, the poller followed no board, and the manager's
//! `<amp_context>` still said `project_id: none`.
//!
//! [`watch`] looks for the marker the way the start does ([`ProjectMarker::find_within`] from the
//! chat's folder, under `AMP_HARNESS_CEILING_DIR`) every [`WATCH_EVERY`]. When what it finds
//! differs from the session's (a new file, another project, the file removed), [`Relink::apply`]
//! moves the session onto it:
//! - **Header, status line, board:** the metadata poller follows the new project
//!   ([`amp_tui::Notifier::project_linked`]), with the `/project` panel's parent-folder note.
//! - **The manager:** its `<amp_context>` names the new project from its next request
//!   ([`crate::reload::ChatReload::relink`]); later `/reload`s keep it.
//! - **Workers:** those dispatched from now on get the new marker ([`Supervisor::set_project`]).
//! - **`/server`:** a switch keeps the new project selected ([`Switcher::set_marker`]).
//! - **Transcript:** `Linked this folder to project #N <name>.`
//!
//! A marker that does not parse (being written, or broken) is ignored until it does. The
//! repository-level configuration found from the marker at start (`.amp/harness`) is not moved.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex, PoisonError},
    time::Duration,
};

use amp_core::{project::ProjectMarker, supervisor::Supervisor};
use amp_tui::{Notifier, ProjectLinked};

use crate::{reload::ChatReload, server_cmd::Switcher};

/// How often [`watch`] looks for the marker.
pub const WATCH_EVERY: Duration = Duration::from_secs(1);

/// What a relink updates.
pub struct Relink {
    pub cwd: PathBuf,
    pub reload: Arc<ChatReload>,
    pub supervisor: Option<Arc<Supervisor>>,
    pub switcher: Arc<Switcher>,
    pub notes: Notifier,
    /// The banner's project text now ([`crate::chat::banner_project`]).
    pub banner: Mutex<String>,
}

impl Relink {
    /// Moves the session onto `marker` (see the module docs).
    pub async fn apply(&self, marker: Option<ProjectMarker>) {
        if let Some(sup) = &self.supervisor {
            sup.set_project(marker.clone());
        }
        self.switcher.set_marker(marker.clone());
        let refreshed = self.reload.relink(marker.clone()).await;
        let shown = crate::chat::banner_project(marker.as_ref(), &self.cwd);
        let was = std::mem::replace(
            &mut *self.banner.lock().unwrap_or_else(PoisonError::into_inner),
            shown.clone(),
        );
        self.notes.project_linked(ProjectLinked {
            project_id: marker.as_ref().map(|m| m.project_id),
            marker: marker.as_ref().and_then(|m| parent_note(m, &self.cwd)),
            notice: linked_line(marker.as_ref(), &self.cwd),
            banner: Some((was, shown)),
        });
        if let Err(e) = refreshed {
            self.notes.error(format!(
                "The manager's context still names the old project ({e}); restart the chat here \
                 to use the new one."
            ));
        }
    }
}

/// The transcript's line for a session now on `marker`.
pub fn linked_line(marker: Option<&ProjectMarker>, cwd: &Path) -> String {
    match marker {
        Some(m) => {
            let name = m
                .project_name
                .as_deref()
                .map(|n| format!(" {n}"))
                .unwrap_or_default();
            let from = if m.in_parent_of(cwd) {
                format!(" (from {})", m.shown_from(cwd))
            } else {
                String::new()
            };
            format!(
                "Linked this folder to project #{}{name}{from}. The header, board and agents use \
                 it now.",
                m.project_id
            )
        }
        None => "This folder has no .amp.json any more: the chat has no project now.".to_owned(),
    }
}

/// The `/project` panel's note for a marker in a parent folder (#2356): its id and where it is.
pub fn parent_note(marker: &ProjectMarker, cwd: &Path) -> Option<(i64, String)> {
    marker.in_parent_of(cwd).then(|| {
        (
            marker.project_id,
            format!(
                "{} (a parent folder's: {})",
                marker.shown_from(cwd),
                marker.path.display()
            ),
        )
    })
}

/// The marker the chat in `cwd` would find now; `Err` when one is there but can't be used.
fn look(cwd: &Path, ceiling: Option<&Path>) -> Result<Option<ProjectMarker>, ()> {
    ProjectMarker::find_within(cwd, ceiling).map_err(|_| ())
}

/// Calls `on_change` with the marker each time the one found from `cwd` differs from the last
/// (starting from `current`, the session's), `every` apart. Runs until the task is aborted.
pub async fn watch<F, Fut>(
    cwd: PathBuf,
    ceiling: Option<PathBuf>,
    mut current: Option<ProjectMarker>,
    every: Duration,
    on_change: F,
) where
    F: Fn(Option<ProjectMarker>) -> Fut,
    Fut: std::future::Future<Output = ()>,
{
    loop {
        tokio::time::sleep(every).await;
        let (dir, ceil) = (cwd.clone(), ceiling.clone());
        let found = tokio::task::spawn_blocking(move || look(&dir, ceil.as_deref()))
            .await
            .unwrap_or(Err(()));
        if let Ok(found) = found
            && found != current
        {
            current = found.clone();
            on_change(found).await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/relink")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("play/app")).unwrap();
        std::fs::canonicalize(&dir).unwrap()
    }

    fn marker(dir: &Path, id: i64, name: &str) -> ProjectMarker {
        let path = dir.join(".amp.json");
        let text = format!(r#"{{"project_id": {id}, "project_name": "{name}"}}"#);
        std::fs::write(&path, &text).unwrap();
        ProjectMarker::parse(&path, &text).unwrap()
    }

    #[test]
    fn the_line_names_the_project_and_a_parent_folders_marker() {
        let dir = scratch("line");
        let app = dir.join("play/app");
        let here = marker(&app, 159, "Go API");
        assert_eq!(
            linked_line(Some(&here), &app),
            "Linked this folder to project #159 Go API. The header, board and agents use it now."
        );
        assert_eq!(parent_note(&here, &app), None);
        let up = marker(&dir.join("play"), 45, "Harness play");
        let line = linked_line(Some(&up), &app);
        assert!(
            line.starts_with("Linked this folder to project #45 Harness play (from ../.amp.json)"),
            "{line}"
        );
        let (id, note) = parent_note(&up, &app).unwrap();
        assert_eq!(id, 45);
        assert!(
            note.starts_with("../.amp.json (a parent folder's: "),
            "{note}"
        );
        assert!(linked_line(None, &app).contains("no project now"));
    }

    #[tokio::test]
    async fn the_watcher_reports_a_new_changed_or_removed_marker_once() {
        let dir = scratch("watch");
        let app = dir.join("play/app");
        let seen: Arc<Mutex<Vec<Option<i64>>>> = Arc::default();
        let sink = Arc::clone(&seen);
        let task = tokio::spawn(watch(
            app.clone(),
            Some(dir.clone()),
            None,
            Duration::from_millis(30),
            move |m: Option<ProjectMarker>| {
                let sink = Arc::clone(&sink);
                async move { sink.lock().unwrap().push(m.map(|m| m.project_id)) }
            },
        ));
        let wait_for = |n: usize| {
            let seen = Arc::clone(&seen);
            async move {
                let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
                while seen.lock().unwrap().len() < n && tokio::time::Instant::now() < deadline {
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
                seen.lock().unwrap().clone()
            }
        };
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert!(seen.lock().unwrap().is_empty(), "no marker, no change");
        // Half-written: not JSON yet, so nothing happens until it is.
        std::fs::write(app.join(".amp.json"), "{\"project_id\": ").unwrap();
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert!(seen.lock().unwrap().is_empty());
        marker(&app, 159, "Go API");
        assert_eq!(wait_for(1).await, vec![Some(159)]);
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert_eq!(
            seen.lock().unwrap().len(),
            1,
            "the same marker again is no change"
        );
        marker(&app, 160, "Go API 2");
        assert_eq!(wait_for(2).await, vec![Some(159), Some(160)]);
        std::fs::remove_file(app.join(".amp.json")).unwrap();
        assert_eq!(wait_for(3).await, vec![Some(159), Some(160), None]);
        task.abort();
        let _ = std::fs::remove_dir_all(&dir);
    }
}
