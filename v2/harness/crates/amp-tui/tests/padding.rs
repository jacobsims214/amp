//! #2251: left and right padding around every area of the chat; #2344: halved at 80 columns or
//! fewer, and a blank row above and below the whole screen (none under 24 rows).
//!
//! The same screens (the AMP header, a user bubble, a reply with a table, a code block and a long
//! paragraph, tool calls, the `/workers` panel, the `/` menu, the editor, the AMP status line and
//! the footer) are rendered at 80 and 127 columns and compared with
//! `tests/snapshots/padding-{80,127}.txt` (plain text; `|` marks the right edge), and at 120x40 and
//! 80x24 with every row kept, `padding-{120x40,80x24}.txt`. Regenerate them
//! with `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test padding` and review the diff. The other
//! tests check that every row keeps the padding clear, that narrow terminals still fit and that
//! `[tui] padding` changes it.
//!
//! The theme is process-global, so every test here takes [`LOCK`].

use std::{
    path::PathBuf,
    sync::{Arc, Mutex, MutexGuard},
    time::Duration,
};

use amp_core::{agent::memory_session, events::ChannelEmitter, meta::MetaHandle};
use amp_tui::{
    AmpTheme, ColorDepth, DEFAULT_PADDING, Gap, MIN_INNER, Padding, SHORT, Shell, ShellInfo,
    Submit, VERTICAL, Variant, chrome::sample, install_theme,
};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use rpi_tui::{strip_ansi, visible_width};
use tokio::sync::mpsc::UnboundedReceiver;

static LOCK: Mutex<()> = Mutex::new(());
const H: usize = 60;
const WAIT: Duration = Duration::from_secs(20);

const REPLY: &str = "Here is the board, with a paragraph long enough that it has to wrap at \
eighty columns and again at a hundred and twenty-seven, so the right edge shows.\n\n\
| ticket | state | owner |\n|---|---|---|\n| #2251 | in progress | backend worker |\n\
| #2252 | ready | backend worker |\n\n\
```rust\nfn padded(width: usize, padding: usize) -> usize { width - 2 * padding } // inner\n```\n\n\
Done.";

fn themed() -> MutexGuard<'static, ()> {
    let guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    install_theme(&AmpTheme::builtin(Variant::Dark, ColorDepth::TrueColor));
    guard
}

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
}

async fn rig() -> Rig {
    let script = FauxScript::new();
    script.set_responses(vec![
        FauxStep::tool_call("ls", serde_json::json!({ "path": "crates" })),
        FauxStep::text(REPLY),
    ]);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: Some("harness-m4".into()),
        session_file: None,
        banner: vec!["AMP harness · manager".into(), "project #2 AMP".into()],
    };
    let (mut shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    shell.set_meta(MetaHandle::stub(
        sample::meta(true),
        Arc::new(|tool, _| Err(format!("{tool}: not in the stub"))),
    ));
    let workers = sample::data(true).workers;
    shell
        .view()
        .chrome()
        .set_workers(Arc::new(move || workers.clone()));
    let mut rig = Rig {
        shell,
        events,
        done,
    };
    rig.say("show me the board").await;
    rig
}

impl Rig {
    async fn say(&mut self, text: &str) {
        assert_eq!(self.shell.submit(text).await, Submit::Continue);
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, WAIT)
            .await
            .expect("idle");
        self.shell.settle(WAIT).await.expect("settled");
    }

    async fn type_text(&mut self, text: &str) {
        let editor = self.shell.view().editor().clone();
        for c in text.chars() {
            let k = KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE);
            if !self.shell.key(&k, editor.get_text().is_empty()).await {
                editor.handle_key(k);
            }
        }
    }

    fn rows(&self, width: usize) -> Vec<String> {
        self.rows_at(width, H)
    }

    fn rows_at(&self, width: usize, height: usize) -> Vec<String> {
        let rows: Vec<String> = self
            .shell
            .view()
            .render_frame(width, height)
            .iter()
            .map(|l| strip_ansi(l))
            .collect();
        // Leading blank rows of the transcript's top are not interesting; the frame always fills.
        assert_eq!(rows.len(), height);
        rows
    }
}

/// Every row leaves `pad` blank columns on the left and right of a `width`-column screen. A
/// box's row (#2882) leaves one column less: its edge takes the padding's inner column, so the
/// text inside it (after the edge and one column of padding) starts a column further in than
/// the padding alone put it.
fn assert_padded(rows: &[String], width: usize, pad: usize) {
    for row in rows {
        let cells = visible_width(row);
        assert!(cells <= width, "{cells} > {width}: {row:?}");
        if row.trim().is_empty() {
            continue;
        }
        let lead = row.len() - row.trim_start_matches(' ').len();
        let boxed = row.trim_start().starts_with(['┌', '│', '└']);
        let side = if boxed { pad.saturating_sub(1) } else { pad };
        assert!(lead >= side, "touches the left edge: {row:?}");
        let used = visible_width(row.trim_end());
        assert!(
            used <= width - side,
            "touches the right edge ({used}): {row:?}"
        );
        if boxed {
            assert_eq!(
                lead, side,
                "a box sits one column inside the padding: {row:?}"
            );
        }
    }
}

/// Every row, with `|` at the right edge (blank rows kept, so the top and bottom padding show).
fn full_snapshot_text(rows: &[String], width: usize) -> String {
    let mut out = String::new();
    for row in rows {
        let row = row.trim_end();
        let fill = width.saturating_sub(visible_width(row));
        out.push_str(&mask_times(&format!("{row}{}|", " ".repeat(fill))));
        out.push('\n');
    }
    out
}

/// The rows that show something, with `|` at the right edge.
fn snapshot_text(rows: &[String], width: usize) -> String {
    let mut out = String::new();
    let mut blank = 0;
    for row in rows {
        let row = row.trim_end();
        if row.is_empty() {
            blank += 1;
            // Collapse the transcript's empty top into one row.
            if blank > 1 && out.is_empty() {
                continue;
            }
        } else {
            blank = 0;
        }
        let fill = width.saturating_sub(visible_width(row));
        out.push_str(&mask_times(&format!("{row}{}|", " ".repeat(fill))));
        out.push('\n');
    }
    out
}

/// Run times (`497988h41m`, `12 ms`) as `#`, keeping the row's width, so the snapshot doesn't
/// depend on the clock.
fn mask_times(row: &str) -> String {
    let chars: Vec<char> = row.chars().collect();
    let mut out = String::with_capacity(row.len());
    let mut i = 0;
    while i < chars.len() {
        if chars[i].is_ascii_digit() && (i == 0 || !chars[i - 1].is_alphanumeric()) {
            let mut j = i;
            while j < chars.len() && (chars[j].is_ascii_digit() || "hms ".contains(chars[j])) {
                j += 1;
            }
            let run: String = chars[i..j].iter().collect();
            let unit = run.trim_end();
            if unit.ends_with('m') || unit.ends_with("ms") || unit.ends_with('s') {
                for c in &chars[i..i + unit.chars().count()] {
                    out.push(if c.is_ascii_digit() { '#' } else { *c });
                }
                i += unit.chars().count();
                continue;
            }
        }
        out.push(chars[i]);
        i += 1;
    }
    out
}

fn check_snapshot(name: &str, got: &str) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("{name}.txt"));
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::write(&path, got).unwrap();
        return;
    }
    let want = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "{}: {e} (run with UPDATE_SNAPSHOTS=1 to create it)",
            path.display()
        )
    });
    assert_eq!(got, want, "{name} differs (UPDATE_SNAPSHOTS=1 to accept)");
}

// The theme lock is a std Mutex held for the whole test: no other task on this runtime takes it.
#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_area_is_padded_at_80_and_127_columns() {
    let _g = themed();
    let mut rig = rig().await;
    assert_eq!(rig.shell.view().padding(), DEFAULT_PADDING);
    rig.say("/workers").await;
    rig.type_text("/q").await;
    assert!(rig.shell.view().menu().is_open());
    for width in [80, 127] {
        let rows = rig.rows(width);
        let text = rows.join("\n");
        for part in [
            "ada@example.com",        // header
            "show me the board",      // user bubble
            "ls crates",              // tool line
            "#2251",                  // table
            "fn padded",              // code block
            "Done.",                  // after it
            "/questions",             // the menu
            "QUESTIONS",              // AMP status line
            "/work/amp (harness-m4)", // footer
        ] {
            assert!(text.contains(part), "{part} at {width}:\n{text}");
        }
        assert_padded(&rows, width, Padding::new(DEFAULT_PADDING).at(width));
        check_snapshot(&format!("padding-{width}"), &snapshot_text(&rows, width));
    }
}

/// #2344: padding on the sides at 120x40 and 80x24: 2 columns left and right at 120, 1 at 80,
/// and a blank row at the top of both (#2882: none at the bottom; the boxes separate the screen).
#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_side_is_padded_at_120x40_and_80x24() {
    let _g = themed();
    let mut rig = rig().await;
    rig.say("/workers").await;
    for (width, height, side) in [(120, 40, 2), (80, 24, 1)] {
        let rows = rig.rows_at(width, height);
        assert_eq!(rig.shell.view().vertical_padding(), VERTICAL);
        assert_eq!(rows[0].trim(), "", "a blank top row at {width}x{height}");
        // The header (the Conversation box's top edge, #2882) is right under the top row, the
        // footer is the last row.
        assert!(rows[1].contains("ada@example.com"), "{rows:#?}");
        assert!(rows[height - 1].contains("thinking off"), "{rows:#?}");
        assert_padded(&rows, width, side);
        let left = |row: &str| row.len() - row.trim_start_matches(' ').len();
        assert_eq!(left(&rows[1]), side - 1, "{rows:#?}");
        assert_eq!(left(&rows[height - 1]), side, "{rows:#?}");
        check_snapshot(
            &format!("padding-{width}x{height}"),
            &full_snapshot_text(&rows, width),
        );
    }
    // Shorter than 24 rows: no blank rows, the footer is the last row.
    let rows = rig.rows_at(80, SHORT - 1);
    assert_eq!(Gap::rows_at(SHORT - 1), 0);
    assert!(rows[0].contains("ada@example.com"), "{rows:#?}");
    assert!(rows[SHORT - 2].contains("thinking off"), "{rows:#?}");
}

#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn narrow_terminals_shrink_the_padding_and_still_fit() {
    let _g = themed();
    let mut rig = rig().await;
    rig.say("/workers").await;
    for width in [24usize, 30, 40, 41, 42, 43, 44, 50, 60] {
        let pad = Padding::new(DEFAULT_PADDING).at(width);
        let rows = rig.rows(width);
        assert_padded(&rows, width, pad);
        if width <= MIN_INNER + 1 {
            // No padding: something reaches column 0.
            assert!(
                rows.iter()
                    .any(|r| !r.trim().is_empty() && !r.starts_with(' ')),
                "{width}: {rows:#?}"
            );
        }
    }
}

#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_padding_setting_moves_every_area() {
    let _g = themed();
    let mut rig = rig().await;
    for pad in [0, 1, 4] {
        rig.shell.set_padding(pad);
        assert_eq!(rig.shell.view().padding(), pad);
        let rows = rig.rows(100);
        assert_padded(&rows, 100, pad);
        let left = |needle: &str| {
            rows.iter()
                .find(|r| r.contains(needle))
                .map(|r| r.len() - r.trim_start_matches(' ').len())
                .unwrap_or_else(|| panic!("{needle}: {rows:#?}"))
        };
        // The footer starts right at the padding; the header is the Conversation box's top edge,
        // one column further out (#2882).
        assert_eq!(left("ada@example.com"), pad.saturating_sub(1), "{rows:#?}");
        assert_eq!(left("/work/amp"), pad, "{rows:#?}");
    }
}
