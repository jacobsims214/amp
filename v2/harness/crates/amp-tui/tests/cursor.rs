//! #2344: the input's cursor. While the input has focus the editor marks its insertion point,
//! which the terminal shows as a blinking bar ([`amp_tui::terminal`]); a panel with the keys hides
//! it; a click into the editor box focuses it and moves the insertion point. Positions are
//! checked with multi-line and wide-character input at 120x40 and 80x24 (the padding on every
//! side moves the cursor with it). The terminal's escape sequences are checked on a real pty by
//! `crates/amp-harness/tests/chat_cursor.rs`.

use std::sync::Arc;

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{ChatView, ModelChoice, Shell, ShellInfo, Submit};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use tokio::sync::mpsc::UnboundedReceiver;

/// The editor's text row: the one the cursor is on with an empty editor.
fn text_row(view: &ChatView, width: usize, height: usize) -> usize {
    view.cursor_position(width, height).expect("a cursor").0
}

#[test]
fn the_cursor_sits_at_the_insertion_point_inside_the_padding() {
    for (width, height, side) in [(120, 40, 2), (80, 24, 1)] {
        let view = ChatView::new();
        let row = text_row(&view, width, height);
        // The editor's text row is right above its bottom rule; the blank bottom row stays
        // blank.
        let rows = view.render_plain(width, height);
        assert!(rows[row + 1].trim_start().starts_with('─'), "{rows:#?}");
        assert_eq!(rows[height - 1], "", "{rows:#?}");
        assert_eq!(view.cursor_position(width, height), Some((row, side)));

        // Wide characters take two columns each.
        view.editor().insert("héllo 世界");
        assert_eq!(
            view.cursor_position(width, height),
            Some((row, side + 6 + 4)),
            "{width}x{height}"
        );

        // A second line: the editor grows by a row, so its text starts a row higher, and the
        // cursor is on the second line.
        view.editor().insert("\nab");
        let (r, c) = view.cursor_position(width, height).unwrap();
        assert_eq!(c, side + 2, "{width}x{height}");
        let rows = view.render_plain(width, height);
        assert!(rows[r - 1].contains("héllo 世界"), "{rows:#?}");
        assert!(rows[r].trim_start().starts_with("ab"), "{rows:#?}");
    }
}

#[test]
fn the_cursor_follows_wrapped_input() {
    let view = ChatView::new();
    let (width, height, side) = (80, 24, 1);
    // 78 inner columns; the cursor's line wraps at 77, so 80 characters is two rows.
    view.editor().insert(&"x".repeat(80));
    let (r, c) = view.cursor_position(width, height).unwrap();
    assert_eq!(c, side + 3);
    let rows = view.render_plain(width, height);
    assert_eq!(rows[r - 1].trim(), "x".repeat(77), "{rows:#?}");
}

#[test]
fn no_cursor_without_input_focus() {
    let view = ChatView::new();
    assert!(view.cursor_position(120, 40).is_some());
    view.set_input_focus(false);
    assert_eq!(view.cursor_position(120, 40), None);
    // The placeholder still shows.
    assert!(
        view.render_plain(120, 40)
            .join("\n")
            .contains("/ for commands")
    );
    view.set_input_focus(true);
    assert!(view.cursor_position(120, 40).is_some());
}

#[test]
fn a_click_maps_to_the_text_under_it() {
    let (width, height, side) = (120, 40, 2);
    let view = ChatView::new();
    view.editor().insert("hello world\n世界x");
    let (second, _) = view.cursor_position(width, height).unwrap();
    let first = second - 1;
    // On the first line, at "w".
    assert_eq!(
        view.editor_hit(width, height, first, side + 6),
        Some((0, 6))
    );
    // Past the end of a line: its end.
    assert_eq!(
        view.editor_hit(width, height, first, side + 40),
        Some((0, 11))
    );
    // Inside the padding: the line's start.
    assert_eq!(view.editor_hit(width, height, first, 0), Some((0, 0)));
    // On the second half of 世: before it. On 界: after 世 (3 bytes).
    assert_eq!(
        view.editor_hit(width, height, second, side + 1),
        Some((1, 0))
    );
    assert_eq!(
        view.editor_hit(width, height, second, side + 2),
        Some((1, 3))
    );
    // The rules above and below go to the nearest line.
    assert_eq!(
        view.editor_hit(width, height, first - 1, side + 1),
        Some((0, 1))
    );
    assert_eq!(
        view.editor_hit(width, height, second + 1, side + 5),
        Some((1, 7))
    );
    // Outside the box: nothing.
    assert_eq!(view.editor_hit(width, height, 0, side), None);
    assert_eq!(view.editor_hit(width, height, height - 1, side), None);
}

struct Rig {
    shell: Shell,
    _events: UnboundedReceiver<AgentEvent>,
    _done: UnboundedReceiver<amp_tui::RunDone>,
}

async fn rig() -> Rig {
    let provider = FauxProvider::new(FauxScript::new());
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
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let mut alt = model.clone();
    alt.id = "faux-2".into();
    let models = vec![
        ModelChoice {
            reference: format!("{}/{}", model.provider, model.id),
            model: model.clone(),
        },
        ModelChoice {
            reference: format!("{}/faux-2", model.provider),
            model: alt,
        },
    ];
    let (shell, done) = Shell::new(Arc::new(harness), info, models);
    Rig {
        shell,
        _events: events,
        _done: done,
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_panel_with_the_keys_hides_the_cursor_and_a_click_or_closing_it_brings_it_back() {
    let (width, height) = (120, 40);
    let mut rig = rig().await;
    rig.shell.sync_focus();
    assert!(rig.shell.input_focused(), "focused on start");
    assert!(rig.shell.view().cursor_position(width, height).is_some());

    // /model opens a picker: ↑↓ and Enter go to it, so no cursor.
    assert_eq!(rig.shell.submit("/model").await, Submit::Continue);
    rig.shell.view().editor().clear();
    rig.shell.sync_focus();
    assert!(rig.shell.view().panel().is_open());
    assert!(!rig.shell.input_focused());
    assert_eq!(rig.shell.view().cursor_position(width, height), None);

    // Esc closes it: the cursor is back.
    assert!(rig.shell.key(&key(KeyCode::Esc), true).await);
    rig.shell.sync_focus();
    assert!(rig.shell.input_focused());
    assert!(rig.shell.view().cursor_position(width, height).is_some());

    // Open it again and click into the editor: the editor has the keys, ↓ no longer moves the
    // panel's selection.
    assert_eq!(rig.shell.submit("/model").await, Submit::Continue);
    rig.shell.view().editor().clear();
    rig.shell.sync_focus();
    assert!(!rig.shell.input_focused());
    let selected = rig.shell.view().panel().state().unwrap().selected;
    // Find the editor's text row with focus on, then hand the keys back to the panel.
    rig.shell.view().set_input_focus(true);
    let (row, col) = rig.shell.view().cursor_position(width, height).unwrap();
    rig.shell.sync_focus();
    // A click outside the editor does nothing.
    assert!(!rig.shell.click(width, height, 0, 5));
    assert!(!rig.shell.input_focused());
    assert!(rig.shell.click(width, height, row, col + 10));
    assert!(rig.shell.input_focused());
    assert_eq!(
        rig.shell.view().cursor_position(width, height),
        Some((row, col))
    );
    assert!(
        !rig.shell.key(&key(KeyCode::Down), true).await,
        "the editor's key"
    );
    assert_eq!(rig.shell.view().panel().state().unwrap().selected, selected);
    // Esc still closes the panel; the editor keeps the keys.
    assert!(rig.shell.key(&key(KeyCode::Esc), true).await);
    rig.shell.sync_focus();
    assert!(!rig.shell.view().panel().is_open());
    assert!(rig.shell.input_focused());

    // Typing into the editor while a panel is open gives it the keys too.
    assert_eq!(rig.shell.submit("/model").await, Submit::Continue);
    rig.shell.view().editor().clear();
    rig.shell.view().editor().insert("hi");
    rig.shell.sync_focus();
    assert!(rig.shell.input_focused());
}
