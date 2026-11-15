//! Select-to-copy's highlight layer and `ChatView` selection API (story #580, #4215).
//!
//! The transcript's rows carry #4210's in-band row markers; the selection layer between the
//! scroll view and the inset takes them off, paints the selection in reverse video in content
//! space (so it scrolls with the text), and extracts the selected text as shown. Everything here
//! goes through `ChatView::render_frame`: no terminal, no clipboard.

use std::sync::Arc;

use amp_tui::scroll::WHEEL_LINES;
use amp_tui::selection::Pos;
use amp_tui::wrap::{encode_row_meta, hard_wrap_marked, reflow_row_marked, wrap_marked};
use amp_tui::{ChatView, SessionParts};
use rpi_tui::{Component, Container, visible_width};

const W: usize = 80;
const H: usize = 30;
const REV: &str = "\x1b[7m";

/// How a test block wraps its text.
#[derive(Clone, Copy)]
enum Wrap {
    Words,
    Hard,
    Reflow,
}

/// A transcript block that wraps its text the way the transcript does and marks each row,
/// as #4214's components do.
struct Marked(String, Wrap);

impl Component for Marked {
    fn render(&self, width: usize) -> Vec<String> {
        let (rows, meta) = match self.1 {
            Wrap::Words => wrap_marked(&self.0, width),
            Wrap::Hard => hard_wrap_marked(&self.0, width),
            Wrap::Reflow => reflow_row_marked(&self.0, width),
        };
        rows.into_iter()
            .zip(meta)
            .map(|(row, m)| format!("{row}{}", encode_row_meta(&m)))
            .collect()
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

fn block(text: &str, wrap: Wrap) -> Arc<dyn Component> {
    Arc::new(Marked(text.to_owned(), wrap))
}

fn view_with(blocks: &[(&str, Wrap)]) -> ChatView {
    let view = ChatView::new();
    for (text, wrap) in blocks {
        view.add_to_transcript(block(text, *wrap));
    }
    view
}

/// The content cell of the first screen cell showing `needle`.
fn locate(view: &ChatView, width: usize, needle: &str) -> Pos {
    let frame = view.render_plain(width, H);
    let (row, line) = frame
        .iter()
        .enumerate()
        .find(|(_, l)| l.contains(needle))
        .unwrap_or_else(|| panic!("{needle:?} not on screen:\n{}", frame.join("\n")));
    let col = visible_width(&line[..line.find(needle).unwrap()]);
    view.content_pos(width, H, row, col)
        .unwrap_or_else(|| panic!("{needle:?} at ({row}, {col}) is outside the transcript"))
}

fn right(p: Pos, cols: usize) -> Pos {
    Pos::new(p.row, p.col + cols)
}

fn select(view: &ChatView, from: Pos, to: Pos) {
    view.select_start(from);
    view.select_extend(to);
}

/// The screen rows showing the highlight.
fn highlighted(view: &ChatView, width: usize) -> Vec<usize> {
    view.render_frame(width, H)
        .iter()
        .enumerate()
        .filter(|(_, l)| l.contains(REV))
        .map(|(i, _)| i)
        .collect()
}

fn frame_has(view: &ChatView, needle: &str) -> bool {
    view.render_frame(W, H).iter().any(|l| l.contains(needle))
}

#[test]
fn ascii_selection_paints_and_copies_exact_text() {
    let view = view_with(&[("hello world, said the transcript", Wrap::Words)]);
    let at = locate(&view, W, "world");
    select(&view, at, right(at, 4));
    assert_eq!(view.select_text().as_deref(), Some("world"));
    assert!(frame_has(&view, "\x1b[7mworld\x1b[27m"));
    assert_eq!(highlighted(&view, W).len(), 1);
    // Backwards drag selects the same cells.
    select(&view, right(at, 4), at);
    assert_eq!(view.select_text().as_deref(), Some("world"));
}

#[test]
fn cjk_selection_snaps_to_whole_glyphs() {
    let view = view_with(&[("前 日本語テキスト 後", Wrap::Words)]);
    let at = locate(&view, W, "日本語");
    // From the right half of 日 to the right half of 本.
    select(&view, right(at, 1), right(at, 3));
    assert_eq!(view.select_text().as_deref(), Some("日本"));
    assert!(frame_has(&view, "\x1b[7m日本\x1b[27m"));
}

#[test]
fn emoji_selection_snaps_to_whole_glyphs() {
    let view = view_with(&[("go 😀 now", Wrap::Words)]);
    let at = locate(&view, W, "😀");
    // The emoji's right half to the `n` after it.
    select(&view, right(at, 1), right(at, 3));
    assert_eq!(view.select_text().as_deref(), Some("😀 n"));
    assert!(frame_has(&view, "\x1b[7m😀 n\x1b[27m"));
}

#[test]
fn wrapped_paragraph_rejoins_and_highlights_every_row() {
    let words: Vec<String> = (0..40).map(|i| format!("w{i:02}")).collect();
    let text = words.join(" ");
    let view = view_with(&[(&text, Wrap::Words)]);
    let start = locate(&view, W, "w00");
    let end = locate(&view, W, "w39");
    assert!(end.row > start.row + 1, "the paragraph wraps");
    select(&view, start, right(end, 2));
    assert_eq!(view.select_text().as_deref(), Some(text.as_str()));
    assert_eq!(highlighted(&view, W).len(), end.row - start.row + 1);

    // A hard-wrapped (code) line rejoins byte-exact, inner spaces included.
    let code = format!("let  x = \"{}\";", "a".repeat(120));
    let view = view_with(&[(&code, Wrap::Hard)]);
    let start = locate(&view, W, "let  x");
    select(&view, start, Pos::new(start.row + 10, 0));
    assert_eq!(view.select_text().as_deref(), Some(code.as_str()));

    // A list item's continuation rows skip their hanging indent.
    let item = format!("• {}", words[..30].join(" "));
    let view = view_with(&[(&item, Wrap::Reflow)]);
    let start = locate(&view, W, "• w00");
    select(&view, start, Pos::new(start.row + 10, 0));
    assert_eq!(view.select_text().as_deref(), Some(item.as_str()));
}

#[test]
fn highlight_moves_with_scroll() {
    let lines: Vec<String> = (0..120)
        .map(|i| format!("line {i:03} of the log"))
        .collect();
    let blocks: Vec<(&str, Wrap)> = lines.iter().map(|l| (l.as_str(), Wrap::Words)).collect();
    let view = view_with(&blocks);
    view.render_frame(W, H);
    view.scroll().scroll_by(-20);
    let at = locate(&view, W, "line 090");
    select(&view, at, right(at, 7));
    let before = highlighted(&view, W);
    assert_eq!(before.len(), 1);

    view.scroll().scroll_by(-WHEEL_LINES);
    let after = highlighted(&view, W);
    assert_eq!(after, vec![before[0] + WHEEL_LINES as usize]);
    let row = &view.render_plain(W, H)[after[0]];
    assert!(row.contains("line 090"), "{row:?}");
    assert_eq!(view.select_text().as_deref(), Some("line 090"));
    // The screen-to-content mapping follows the scroll too.
    assert_eq!(locate(&view, W, "line 090"), at);
}

#[test]
fn worker_session_selection_extracts_that_workers_text() {
    let view = view_with(&[("manager text here", Wrap::Words)]);
    let worker = view_with(&[("worker says hello", Wrap::Words)]);
    view.show_session(Some(SessionParts {
        transcript: worker.transcript_component(),
        bar: Arc::new(Container::new()),
        keys: Arc::new(Container::new()),
    }));
    let at = locate(&view, W, "worker says");
    select(&view, at, right(at, 10));
    assert_eq!(view.select_text().as_deref(), Some("worker says"));
    assert!(frame_has(&view, "\x1b[7mworker says\x1b[27m"));
}

#[test]
fn no_row_marker_reaches_the_frame() {
    let view = view_with(&[
        ("plain words wrapped across a row or two", Wrap::Words),
        ("code_row_that_is_hard_wrapped", Wrap::Hard),
        ("• a list item", Wrap::Reflow),
    ]);
    for width in [W, 50] {
        let frame = view.render_frame(width, H);
        assert!(frame.iter().all(|l| !l.contains("\x1b_amp:")), "{frame:?}");
        assert!(
            view.render_plain(width, H)
                .iter()
                .any(|l| l.contains("plain words"))
        );
    }
    let at = locate(&view, W, "plain");
    select(&view, at, right(at, 4));
    assert!(
        view.render_frame(W, H)
            .iter()
            .all(|l| !l.contains("\x1b_amp:"))
    );
}

#[test]
fn a_press_without_a_drag_selects_nothing() {
    let view = view_with(&[("hello world", Wrap::Words)]);
    let at = locate(&view, W, "world");
    select(&view, at, at);
    assert_eq!(view.selection(), None);
    assert_eq!(view.select_text(), None);
    assert!(highlighted(&view, W).is_empty());
    // A selection of blanks copies nothing.
    let view = view_with(&[("zq", Wrap::Words)]);
    let at = locate(&view, W, "zq");
    select(&view, right(at, 3), right(at, 9));
    assert_eq!(view.select_text(), None);
}

#[test]
fn reflowing_the_transcript_clears_the_selection() {
    let view = view_with(&[("hello world", Wrap::Words)]);
    let selected = |view: &ChatView| {
        let at = locate(view, W, "world");
        select(view, at, right(at, 4));
        assert!(view.selection().is_some());
    };

    selected(&view);
    view.clear_selection();
    assert_eq!(view.selection(), None);

    selected(&view);
    view.toggle_tools();
    assert_eq!(view.selection(), None);
    assert!(highlighted(&view, W).is_empty());

    selected(&view);
    view.show_session(None);
    assert_eq!(view.selection(), None);

    selected(&view);
    view.render_frame(W + 10, H);
    assert_eq!(view.selection(), None, "a width change clears it");

    let mut view = view;
    selected(&view);
    view.clear_transcript();
    assert_eq!(view.selection(), None);
}

#[test]
fn content_pos_is_none_outside_the_transcript_and_clamps_for_drags() {
    let view = view_with(&[("hello", Wrap::Words)]);
    let at = locate(&view, W, "hello");
    // The last screen row is the footer, outside the transcript.
    assert_eq!(view.content_pos(W, H, H - 1, 0), None);
    let clamped = view.content_pos_clamped(W, H, 0, 0).expect("a transcript");
    assert_eq!(clamped.col, 0);
    assert_eq!(clamped.row, view.scroll().top());
    assert!(clamped.row <= at.row);
}

/// With #4214's real transcript components: a wrapped user message and a wrapped notice copy
/// as their source text, and no marker reaches the frame.
#[test]
fn real_transcript_blocks_strip_markers_and_rejoin() {
    let words: Vec<String> = (0..36).map(|i| format!("u{i:02}")).collect();
    let said = words.join(" ");
    let note: String = (0..36)
        .map(|i| format!("n{i:02}"))
        .collect::<Vec<_>>()
        .join(" ");
    let view = ChatView::new();
    view.add_user(&said);
    view.add_notice(&note);
    for width in [W, 60] {
        let frame = view.render_frame(width, H);
        assert!(frame.iter().all(|l| !l.contains("\x1b_amp:")), "{frame:?}");
    }
    for (text, first, last) in [(&said, "u00", "u35"), (&note, "n00", "n35")] {
        let start = locate(&view, W, first);
        let end = locate(&view, W, last);
        assert!(end.row > start.row, "{text} wraps");
        select(&view, start, right(end, 2));
        assert_eq!(view.select_text().as_deref(), Some(text.as_str()));
    }
}

/// #4214's user bubble breaks a long token mid-word over its background fill: the fill is not
/// copied, so the token rejoins exactly.
#[test]
fn long_word_in_the_user_bubble_rejoins_without_fill() {
    let token = format!("https://example.com/{}", "abcdefghij".repeat(15));
    let view = ChatView::new();
    view.add_user(&format!("zsee {token} okz"));
    let start = locate(&view, W, "zsee");
    let end = locate(&view, W, "okz");
    assert!(end.row > start.row + 1, "the token wraps");
    select(&view, start, right(end, 2));
    assert_eq!(
        view.select_text().as_deref(),
        Some(format!("zsee {token} okz").as_str())
    );
}

// ---------------------------------------------------------------------------------------------
// AMP #4217: the shell's mouse wiring. Press, drag and release go through `Shell::mouse` as the
// terminal loop hands them on; the clipboard is a recording stand-in (a `pbcopy` script in a
// temporary directory plus an OSC 52 sink), never the real one.
// ---------------------------------------------------------------------------------------------

mod shell {
    use std::{
        path::{Path, PathBuf},
        sync::{Arc, Mutex},
        time::Duration,
    };

    use amp_core::{agent::memory_session, events::ChannelEmitter};
    use amp_tui::clipboard::osc52;
    use amp_tui::{Desktop, Platform, Shell, ShellInfo, Submit};
    use crossterm::event::{
        KeyCode, KeyEvent, KeyModifiers, MouseButton, MouseEvent, MouseEventKind,
    };
    use rpi_agent::AgentEvent;
    use rpi_ai::{
        ThinkingLevel,
        providers::faux::{FauxBlock, FauxProvider, FauxScript, FauxStep, faux_assistant_message},
        types::StopReason,
    };
    use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
    use rpi_tui::visible_width;
    use tokio::sync::mpsc::UnboundedReceiver;

    const W: usize = 80;
    const H: usize = 30;
    const WAIT: Duration = Duration::from_secs(20);

    /// One paragraph that wraps over several rows at 80 columns.
    const PARA: &str = "alpha bravo charlie delta echo foxtrot golf hotel india juliett kilo lima \
                        mike november oscar papa quebec romeo sierra tango uniform victor whiskey \
                        xray yankee zulu";

    struct Rig {
        shell: Shell,
        _events: UnboundedReceiver<AgentEvent>,
        _done: UnboundedReceiver<amp_tui::RunDone>,
        dir: PathBuf,
        stubs: PathBuf,
        terminal: Arc<Mutex<Vec<String>>>,
    }

    impl Drop for Rig {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn stub(dir: &Path, name: &str, body: &str) {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    /// A chat that answered `reply` (with thinking first when `thinking` is set), its desktop
    /// a recording stand-in.
    async fn rig(tag: &str, thinking: Option<&str>, reply: &str) -> Rig {
        let mut blocks = Vec::new();
        if let Some(t) = thinking {
            blocks.push(FauxBlock::Thinking(t.to_owned()));
        }
        blocks.push(FauxBlock::Text(reply.to_owned()));
        let script = FauxScript::new();
        script.set_responses(vec![FauxStep::message(faux_assistant_message(
            blocks,
            StopReason::Stop,
        ))]);
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
            git_branch: None,
            session_file: None,
            banner: vec!["AMP harness · manager".into()],
        };
        let (mut shell, mut done) = Shell::new(Arc::new(harness), info, Vec::new());
        shell.set_screen(W, H);
        let dir = std::env::temp_dir().join(format!(
            "amp-select-{tag}-{}-{:?}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let stubs = dir.join("bin");
        std::fs::create_dir_all(&stubs).unwrap();
        stub(&stubs, "pbcopy", "cat > \"$0.$$.out\"");
        let terminal = Arc::new(Mutex::new(Vec::new()));
        let sink = Arc::clone(&terminal);
        // macOS's tool on every OS, so Linux runs the same stand-in.
        shell.set_desktop(
            Desktop::with_path(Platform::Mac, &stubs).with_terminal(Arc::new(move |s: &str| {
                sink.lock().unwrap().push(s.to_owned())
            })),
        );
        let mut events = events;
        assert_eq!(shell.submit("go").await, Submit::Continue);
        shell
            .drive_until_idle(&mut events, &mut done, WAIT)
            .await
            .expect("idle");
        let rig = Rig {
            shell,
            _events: events,
            _done: done,
            dir,
            stubs,
            terminal,
        };
        rig.rows();
        rig
    }

    impl Rig {
        fn rows(&self) -> Vec<String> {
            self.shell.view().render_plain(W, H)
        }

        fn screen(&self) -> String {
            self.rows().join("\n")
        }

        /// The screen cell `(row, column)` of the first `needle` (its first character).
        fn find(&self, needle: &str) -> (usize, usize) {
            let rows = self.rows();
            let (row, line) = rows
                .iter()
                .enumerate()
                .find(|(_, l)| l.contains(needle))
                .unwrap_or_else(|| panic!("{needle:?} not on screen:\n{}", rows.join("\n")));
            (row, visible_width(&line[..line.find(needle).unwrap()]))
        }

        fn mouse(&mut self, kind: MouseEventKind, (row, column): (usize, usize)) -> bool {
            let used = self.shell.mouse(
                W,
                H,
                &MouseEvent {
                    kind,
                    column: column as u16,
                    row: row as u16,
                    modifiers: KeyModifiers::NONE,
                },
            );
            self.rows();
            used
        }

        fn down(&mut self, at: (usize, usize)) -> bool {
            self.mouse(MouseEventKind::Down(MouseButton::Left), at)
        }

        fn drag(&mut self, at: (usize, usize)) -> bool {
            self.mouse(MouseEventKind::Drag(MouseButton::Left), at)
        }

        fn up(&mut self, at: (usize, usize)) -> bool {
            self.mouse(MouseEventKind::Up(MouseButton::Left), at)
        }

        /// What the `pbcopy` stand-in received, one entry per run.
        fn pbcopy(&self) -> Vec<String> {
            let mut out: Vec<String> = std::fs::read_dir(&self.stubs)
                .unwrap()
                .filter_map(Result::ok)
                .filter(|e| {
                    let n = e.file_name().to_string_lossy().into_owned();
                    n.starts_with("pbcopy.") && n.ends_with(".out")
                })
                .map(|e| std::fs::read_to_string(e.path()).unwrap())
                .collect();
            out.sort();
            out
        }

        /// Waits (bounded) for the `pbcopy` stand-in to have received exactly `want`. The
        /// clipboard stops waiting on a slow tool after 2 s and reports it done, so under load
        /// the notice can come before the stand-in has written its file.
        async fn pbcopy_is(&self, want: &[&str]) {
            let deadline = tokio::time::Instant::now() + WAIT;
            while self.pbcopy() != want && tokio::time::Instant::now() < deadline {
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
            assert_eq!(self.pbcopy(), want);
        }

        fn osc52(&self) -> Vec<String> {
            self.terminal
                .lock()
                .unwrap()
                .iter()
                .filter(|s| s.starts_with("\x1b]52;"))
                .cloned()
                .collect()
        }

        /// Polls (bounded) until the screen holds `needle`.
        async fn until(&mut self, needle: &str) {
            let deadline = tokio::time::Instant::now() + WAIT;
            loop {
                self.shell.poll();
                if self.screen().contains(needle) {
                    return;
                }
                assert!(
                    tokio::time::Instant::now() < deadline,
                    "no {needle:?} on screen:\n{}",
                    self.screen()
                );
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }

        /// Gives a background copy time to land (bounded), then polls.
        async fn settle(&mut self) {
            for _ in 0..20 {
                tokio::time::sleep(Duration::from_millis(10)).await;
                self.shell.poll();
            }
        }
    }

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn drag_across_a_wrapped_reply_copies_it_exactly_on_release() {
        let mut r = rig("wrapped", None, PARA).await;
        let start = r.find("alpha");
        let end = r.find("zulu");
        assert!(end.0 > start.0, "the reply wraps:\n{}", r.screen());
        let last = (end.0, end.1 + 3);

        assert!(r.down(start));
        // Nothing is selected or copied while the button is down or dragging.
        assert!(r.shell.view().selection().is_none());
        assert!(r.drag((start.0, start.1 + 4)));
        assert!(r.drag(last));
        assert!(r.shell.view().selection().is_some());
        assert!(r.osc52().is_empty() && r.pbcopy().is_empty());
        assert!(
            r.shell
                .view()
                .render_frame(W, H)
                .iter()
                .any(|l| l.contains("\x1b[7m")),
            "the selection is highlighted"
        );

        assert!(r.up(last));
        assert_eq!(r.osc52(), vec![osc52(PARA)], "one OSC 52 with the text");
        r.until(&format!("Copied {} chars.", PARA.chars().count()))
            .await;
        r.pbcopy_is(&[PARA]).await;
        // The highlight stays after the copy.
        assert!(r.shell.view().selection().is_some());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_backwards_drag_copies_the_same_text() {
        let mut r = rig("backwards", None, PARA).await;
        let start = r.find("charlie");
        let end = r.find("victor");
        let last = (end.0, end.1 + 5);
        r.down(last);
        r.drag(start);
        r.up(start);
        let want = &PARA[PARA.find("charlie").unwrap()..PARA.find("victor").unwrap() + 6];
        assert_eq!(r.osc52(), vec![osc52(want)]);
        r.until("Copied").await;
        r.pbcopy_is(&[want]).await;
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_click_on_a_thinking_header_toggles_it_on_release_and_copies_nothing() {
        let mut r = rig("thinking", Some("why oh why"), "the answer").await;
        let (row, _) = r.find("▸ Thought for ");
        r.down((row, 8));
        // The press alone does nothing: a drag starting here must not open it.
        assert!(r.screen().contains("▸ Thought for "), "{}", r.screen());
        r.up((row, 8));
        assert!(r.screen().contains("▾ Thought for "), "{}", r.screen());
        assert!(r.screen().contains("why oh why"), "{}", r.screen());
        // A drag that starts on the header selects instead and leaves it as it is.
        let (row, _) = r.find("▾ Thought for ");
        r.down((row, 8));
        r.drag((row, 14));
        r.up((row, 14));
        assert!(r.screen().contains("▾ Thought for "), "{}", r.screen());
        assert_eq!(r.osc52().len(), 1);
        r.settle().await;
        assert!(r.shell.view().selection().is_some());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_editor_click_moves_the_cursor_on_press_and_a_selection_never_edits() {
        let mut r = rig("editor", None, PARA).await;
        let editor = r.shell.view().editor().clone();
        editor.handle_paste("draft text");
        r.shell.sync_focus();
        let (row, end) = r.shell.view().cursor_position(W, H).expect("cursor");
        // On the press, as before: no release needed.
        assert!(r.down((row, end - 4)));
        assert_eq!(r.shell.view().cursor_position(W, H), Some((row, end - 4)));
        assert!(r.shell.view().selection().is_none());
        assert!(
            !r.up((row, end - 4)),
            "a release outside the transcript does nothing"
        );

        // Select and copy from the transcript: the editor keeps its text and is never copied.
        let start = r.find("alpha");
        r.down(start);
        r.drag((start.0, start.1 + 10));
        r.up((start.0, start.1 + 10));
        r.until("Copied").await;
        assert_eq!(editor.get_text(), "draft text");
        r.pbcopy_is(&["alpha bravo"]).await;
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn with_mouse_capture_off_nothing_is_selected_or_copied() {
        let mut r = rig("capture-off", None, PARA).await;
        r.shell.set_mouse_capture(false);
        let start = r.find("alpha");
        let end = r.find("zulu");
        r.down(start);
        r.drag(end);
        r.up(end);
        r.settle().await;
        assert!(r.shell.view().selection().is_none());
        assert!(r.osc52().is_empty());
        assert!(r.pbcopy().is_empty());
        assert!(!r.screen().contains("Copied"));
    }

    /// AMP #4218: `[tui] select_copy = false` with capture on is the chat before #4217: a drag
    /// selects and copies nothing, and a click acts on the press (a thinking header opens on
    /// Down, without waiting for Up).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn with_select_copy_off_a_drag_selects_nothing_and_clicks_act_on_press() {
        let mut r = rig("select-off", Some("why oh why"), PARA).await;
        r.shell.set_select_copy(false);
        assert!(r.shell.mouse_capture() && !r.shell.select_copy());
        let start = r.find("alpha");
        let end = r.find("zulu");
        r.down(start);
        r.drag(end);
        r.up(end);
        r.settle().await;
        assert!(r.shell.view().selection().is_none());
        assert!(
            !r.shell
                .view()
                .render_frame(W, H)
                .iter()
                .any(|l| l.contains("\x1b[7m"))
        );
        assert!(r.osc52().is_empty());
        assert!(r.pbcopy().is_empty());
        assert!(!r.screen().contains("Copied"));

        let (row, _) = r.find("▸ Thought for ");
        assert!(r.down((row, 8)), "a press is a click");
        assert!(r.screen().contains("▾ Thought for "), "{}", r.screen());
        assert!(!r.up((row, 8)), "the release does nothing");
        assert!(r.screen().contains("▾ Thought for "), "{}", r.screen());

        // Turning it off drops a selection that was already there.
        r.shell.set_select_copy(true);
        let start = r.find("alpha");
        r.down(start);
        r.drag((start.0, start.1 + 10));
        assert!(r.shell.view().selection().is_some());
        r.shell.set_select_copy(false);
        assert!(r.shell.view().selection().is_none());
        assert!(!r.up((start.0, start.1 + 10)));
        assert!(r.osc52().is_empty());
    }

    /// AMP #4218: the status after a copy gives the count only, never the copied text.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_copy_notice_gives_the_count_never_the_text() {
        let mut r = rig("notice", None, PARA).await;
        let start = r.find("charlie");
        let end = (start.0, start.1 + "charlie delta echo".len() - 1);
        r.down(start);
        r.drag(end);
        r.up(end);
        let want = "charlie delta echo";
        r.pbcopy_is(&[want]).await;
        let notice = format!("Copied {} chars.", want.chars().count());
        r.until(&notice).await;
        let rows = r.rows();
        let lines: Vec<&String> = rows.iter().filter(|l| l.contains("Copied")).collect();
        assert_eq!(lines.len(), 1, "{lines:?}");
        assert_eq!(
            lines[0]
                .trim()
                .trim_matches(|c: char| !c.is_ascii_alphanumeric() && c != '.'),
            notice,
            "{lines:?}"
        );
        for word in want.split(' ') {
            assert!(!lines[0].contains(word), "{word} in {:?}", lines[0]);
        }

        // Every wording copy_notice has (tool, OSC 52 only, failed tool): a count, no text.
        use amp_tui::clipboard::{Copied, copy_notice};
        let cases = [
            Copied {
                tool: Some("pbcopy"),
                osc52: true,
                tool_error: None,
            },
            Copied {
                tool: None,
                osc52: true,
                tool_error: None,
            },
            Copied {
                tool: None,
                osc52: true,
                tool_error: Some("xclip: no display".into()),
            },
            Copied {
                tool: None,
                osc52: false,
                tool_error: Some("xclip: no display".into()),
            },
        ];
        for c in &cases {
            let text = copy_notice(want.chars().count(), c).expect("a notice");
            for word in want.split(' ') {
                assert!(!text.contains(word), "{word} in {text:?}");
            }
        }
        assert_eq!(copy_notice(18, &Copied::default()), None);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn esc_editing_keys_and_the_next_press_clear_it_but_paging_keeps_it() {
        let mut r = rig("clear", None, PARA).await;
        let start = r.find("alpha");
        let select = |r: &mut Rig| {
            r.down(start);
            r.drag((start.0, start.1 + 4));
            assert!(r.shell.view().selection().is_some());
        };
        select(&mut r);
        for code in [
            KeyCode::PageUp,
            KeyCode::PageDown,
            KeyCode::Up,
            KeyCode::End,
        ] {
            assert!(!r.shell.selection_key(&key(code)));
            assert!(r.shell.view().selection().is_some(), "{code:?} keeps it");
        }
        assert!(r.shell.selection_key(&key(KeyCode::Esc)));
        assert!(r.shell.view().selection().is_none());
        // Released after Esc dropped it: neither a copy nor a click.
        assert!(!r.up((start.0, start.1 + 4)));
        select(&mut r);
        assert!(r.shell.selection_key(&key(KeyCode::Char('x'))));
        assert!(r.shell.view().selection().is_none());
        select(&mut r);
        r.up((start.0, start.1 + 4));
        assert!(r.shell.selection_key(&key(KeyCode::Backspace)));
        assert!(r.shell.view().selection().is_none());
        // The next press drops a finished selection.
        select(&mut r);
        r.up((start.0, start.1 + 4));
        r.down(start);
        assert!(r.shell.view().selection().is_none());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn dragging_above_the_transcript_scrolls_and_extends_the_selection() {
        let long: String = (1..=80)
            .map(|i| format!("line {i:02}"))
            .collect::<Vec<_>>()
            .join("\n\n");
        let mut r = rig("autoscroll", None, &long).await;
        assert!(r.shell.view().scroll().at_end());
        let top = r.shell.view().scroll().top();
        assert!(top > 0, "the reply is taller than the screen");
        let at = r.find("line 80");
        let at = (at.0, at.1 + 6);
        // The oldest line in sight before the drag.
        let first: usize = r
            .rows()
            .iter()
            .find_map(|l| {
                let i = l.find("line ")?;
                l[i + 5..i + 7].parse().ok()
            })
            .expect("a line in sight");
        r.down(at);
        // Row 0 is above the transcript's scroll view (the header).
        for _ in 0..5 {
            r.drag((0, at.1));
        }
        assert_eq!(r.shell.view().scroll().top(), top - 5);
        r.up((0, at.1));
        // Scrolled up, the notice lands below the view: wait for the stand-in instead.
        let text = r.shell.view().select_text().expect("selected");
        assert_eq!(r.osc52(), vec![osc52(&text)]);
        r.pbcopy_is(&[text.as_str()]).await;
        assert!(text.ends_with("line 80"), "{text:?}");
        assert!(
            text.contains(&format!("line {:02}", first - 1)),
            "it reaches above the first screen (line {first} was the oldest in sight): {text:?}"
        );
    }
}
