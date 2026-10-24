//! #2182: the `/` completion menu, `/help` as a grouped list, and the editor's placeholder.
//! Real rpi `AgentHarness` on the faux provider; keys go through the editor and `Shell::key`
//! the way the terminal loop sends them; no terminal.
//!
//! `/help` at 80 columns is compared with `tests/snapshots/help-80.txt`; regenerate it with
//! `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test slash` and review the diff.

use std::{collections::HashSet, path::PathBuf, sync::Arc, time::Duration};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{COMMANDS, DRAG_KEY, HelpBlock, KEYS, PLACEHOLDER, Shell, ShellInfo, Submit, keys};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use rpi_tui::Component;
use tokio::sync::{Mutex, mpsc::UnboundedReceiver};

const W: usize = 100;
const H: usize = 40;
/// The width `/help` gets in an 80-column terminal: inside the chat's padding (#2251).
const HELP_80: usize = 80 - 2 * amp_tui::DEFAULT_PADDING;

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<amp_tui::RunDone>,
    /// What the editor submitted (its on_submit callback), as the terminal loop would get it.
    submitted: Arc<std::sync::Mutex<Vec<String>>>,
}

async fn rig() -> Rig {
    let script = FauxScript::new();
    script.set_responses(vec![FauxStep::text("ok")]);
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
        banner: vec!["AMP harness · manager".into()],
    };
    let (shell, done) = Shell::new(Arc::new(harness), info, Vec::new());
    let submitted = Arc::new(std::sync::Mutex::new(Vec::new()));
    let sink = submitted.clone();
    shell.view().editor().on_submit(Arc::new(move |text: &str| {
        sink.lock().unwrap().push(text.to_owned())
    }));
    Rig {
        shell,
        events,
        done,
        submitted,
    }
}

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

impl Rig {
    /// One key as the terminal loop sends it: the shell first, then the editor.
    async fn press(&mut self, code: KeyCode) {
        let editor = self.shell.view().editor().clone();
        let empty = editor.get_text().is_empty();
        let k = key(code);
        if !self.shell.key(&k, empty).await {
            editor.handle_key(k);
        }
    }

    async fn type_text(&mut self, text: &str) {
        for c in text.chars() {
            self.press(KeyCode::Char(c)).await;
        }
    }

    fn text(&self) -> String {
        self.shell.view().editor().get_text()
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    /// The menu's rows (`/name …`), as drawn.
    fn menu_rows(&self) -> Vec<String> {
        let screen = self.shell.view().render_plain(W, H);
        let Some(start) = screen.iter().position(|l| l.contains("── commands")) else {
            return Vec::new();
        };
        // Without the chat's left padding (#2251).
        screen[start + 1..]
            .iter()
            .take_while(|l| !l.contains("Tab complete"))
            .map(|l| {
                l.strip_prefix(&" ".repeat(amp_tui::DEFAULT_PADDING))
                    .unwrap_or(l)
                    .to_owned()
            })
            .collect()
    }

    fn submitted(&self) -> Vec<String> {
        self.submitted.lock().unwrap().clone()
    }

    async fn finish(&mut self) {
        self.shell
            .drive_until_idle(&mut self.events, &mut self.done, Duration::from_secs(20))
            .await
            .expect("idle");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn slash_then_t_filters_to_ticket_and_tab_completes() {
    let mut rig = rig().await;
    assert!(!rig.shell.view().menu().is_open());

    rig.type_text("/").await;
    assert!(rig.shell.view().menu().is_open());
    let rows = rig.menu_rows();
    // Every command fits in at most 8 rows plus a "more" line; AMP's come first.
    assert!(rows[0].contains("› /amp"), "{rows:?}\n{}", rig.screen());
    assert!(
        rows[0].contains("the AMP connection"),
        "with its description: {rows:?}"
    );
    assert!(rows.iter().any(|r| r.contains("more")), "{rows:?}");

    if std::env::var_os("SLASH_SCREENS").is_some() {
        eprintln!("{}\n", rig.screen());
    }
    rig.type_text("t").await;
    if std::env::var_os("SLASH_SCREENS").is_some() {
        eprintln!("{}\n", rig.screen());
    }
    let rows = rig.menu_rows();
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert!(rows[0].starts_with("› /ticket [id|none]"), "{rows:?}");
    assert!(rows[0].contains("the current ticket"), "{rows:?}");

    rig.press(KeyCode::Tab).await;
    assert_eq!(rig.text(), "/ticket ");
    assert!(!rig.shell.view().menu().is_open(), "{}", rig.screen());
    // The cursor is at the end: typing goes after the completion.
    rig.type_text("2108").await;
    assert_eq!(rig.text(), "/ticket 2108");
    assert!(rig.submitted().is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn arrows_choose_and_enter_runs_the_choice() {
    let mut rig = rig().await;
    rig.type_text("/q").await;
    let rows = rig.menu_rows();
    assert!(rows[0].starts_with("› /questions"), "{rows:?}");
    assert!(rows[1].starts_with("  /quit"), "{rows:?}");
    rig.press(KeyCode::Down).await;
    assert!(rig.menu_rows()[1].starts_with("› /quit"));
    rig.press(KeyCode::Up).await;
    rig.press(KeyCode::Up).await;
    assert!(rig.menu_rows()[1].starts_with("› /quit"), "↑ wraps around");

    // `/q` is quit's alias: Enter runs it as typed instead of the selection.
    rig.press(KeyCode::Enter).await;
    assert_eq!(rig.submitted(), ["/q"]);

    let mut rig = self::rig().await;
    rig.type_text("/us").await;
    rig.press(KeyCode::Enter).await;
    assert_eq!(
        rig.submitted(),
        ["/usage"],
        "Enter runs the selected command"
    );
    let text = rig.submitted()[0].clone();
    rig.shell.view().editor().clear();
    assert_eq!(rig.shell.submit(&text).await, Submit::Continue);
    assert!(rig.screen().contains("usage this session"));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn esc_dismisses_until_the_text_changes() {
    let mut rig = rig().await;
    rig.type_text("/pr").await;
    assert!(rig.shell.view().menu().is_open());
    rig.press(KeyCode::Esc).await;
    assert!(!rig.shell.view().menu().is_open());
    assert_eq!(rig.text(), "/pr", "Esc leaves the text");
    // Tab now belongs to the editor, not the menu.
    rig.press(KeyCode::Backspace).await;
    assert!(rig.shell.view().menu().is_open(), "a new text reopens it");
    assert_eq!(rig.menu_rows().len(), 2, "/plan and /project");

    // No match, no menu; text that isn't a command name, no menu.
    rig.shell.view().editor().set_text("/zz");
    assert!(!rig.shell.view().menu().is_open());
    rig.shell.view().editor().set_text("hello /t");
    assert!(!rig.shell.view().menu().is_open());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_empty_editor_shows_the_placeholder() {
    let mut rig = rig().await;
    let editor_row = |rig: &Rig| {
        rig.shell
            .view()
            .render_plain(W, H)
            .into_iter()
            .find(|l| l.contains(PLACEHOLDER))
    };
    let row = editor_row(&rig).unwrap_or_else(|| panic!("no placeholder\n{}", rig.screen()));
    assert!(!row.contains("──"), "inside the editor box: {row:?}");
    let styled = rig
        .shell
        .view()
        .render_frame(W, H)
        .into_iter()
        .find(|l| rpi_tui::strip_ansi(l).contains(PLACEHOLDER))
        .unwrap();
    let dim = rpi_tui::theme().colors.dim.fg(PLACEHOLDER);
    assert!(
        styled.contains(&format!("{}{dim}", rpi_tui::CURSOR_MARKER)),
        "dim, whole, right after the cursor: {styled:?}"
    );

    rig.type_text("hi").await;
    assert!(editor_row(&rig).is_none(), "{}", rig.screen());
    rig.shell.view().editor().clear();
    assert!(editor_row(&rig).is_some());

    // Still there after a reply.
    assert_eq!(rig.shell.submit("hello").await, Submit::Continue);
    rig.finish().await;
    assert!(editor_row(&rig).is_some(), "{}", rig.screen());
}

/// The theme is process-global; /help's render test takes it alone.
static LOCK: Mutex<()> = Mutex::const_new(());

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn help_is_grouped_with_descriptions_and_fits_80_columns() {
    let _lock = LOCK.lock().await;
    let mut rig = rig().await;
    rig.shell.submit("/help").await;
    // Tall enough for the whole list.
    let screen = rig.shell.view().render_plain(W, 80).join("\n");
    for heading in ["AMP", "Config", "Chat", "Keys"] {
        assert!(
            screen.lines().any(|l| l.trim() == heading),
            "{heading}\n{screen}"
        );
    }
    let amp = screen.find("\n   AMP\n").expect("AMP heading");
    let config = screen.find("\n   Config\n").expect("Config heading");
    let chat = screen.find("\n   Chat\n").expect("Chat heading");
    let keys = screen.find("\n   Keys\n").expect("Keys heading");
    assert!(
        amp < config && config < chat && chat < keys,
        "AMP first, then Config, Chat, Keys"
    );

    // Full contrast: AMP commands are drawn in the text colour, never dim.
    let colors = rpi_tui::theme().colors;
    let frame = rig.shell.view().render_frame(W, 80);
    let ticket = frame
        .iter()
        .find(|l| rpi_tui::strip_ansi(l).contains("/ticket [id|none]"))
        .unwrap();
    assert!(ticket.contains(&colors.text.fg("/ticket")), "{ticket:?}");
    assert!(!ticket.contains(&colors.dim.fg("/ticket")), "{ticket:?}");

    // In an 80-column terminal (inside the padding): every row fits, no word is split,
    // descriptions line up in one column, and none wraps onto a second row.
    let rows: Vec<String> = HelpBlock::new()
        .render(HELP_80)
        .iter()
        .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
        .collect();
    let vocab: HashSet<String> = COMMANDS
        .iter()
        .flat_map(|c| {
            c.usage()
                .split_whitespace()
                .chain(c.about.split_whitespace())
                .map(str::to_owned)
                .collect::<Vec<_>>()
        })
        .chain(KEYS.iter().flat_map(|(k, about)| {
            k.split_whitespace()
                .chain(about.split_whitespace())
                .map(str::to_owned)
                .collect::<Vec<_>>()
        }))
        .chain(["AMP", "Config", "Chat", "Keys"].map(str::to_owned))
        .collect();
    for row in &rows {
        assert!(
            rpi_tui::visible_width(row) <= HELP_80,
            "{row:?} is wider than {HELP_80}"
        );
        for word in row.split_whitespace() {
            assert!(
                vocab.contains(word),
                "{word:?} is not a whole word: {row:?}"
            );
        }
    }
    assert!(
        !rows.iter().any(|r| r.starts_with(&" ".repeat(10))),
        "a description wrapped:\n{}",
        rows.join("\n")
    );
    for c in COMMANDS {
        let row = rows
            .iter()
            .find(|r| {
                r.trim_start().starts_with(&format!("{} ", c.usage())) || r.trim() == c.usage()
            })
            .unwrap_or_else(|| panic!("{} missing\n{}", c.usage(), rows.join("\n")));
        let first_word = c.about.split_whitespace().next().unwrap();
        assert!(row.contains(first_word), "{row:?}");
    }
    let column = |r: &String, word: &str| r.find(word).unwrap();
    let amp_row = rows.iter().find(|r| r.contains("/amp ")).unwrap();
    let quit_row = rows.iter().find(|r| r.contains("/quit ")).unwrap();
    let enter_row = rows.iter().find(|r| r.contains("Enter ")).unwrap();
    assert_eq!(column(amp_row, "the AMP"), column(quit_row, "leave"));
    assert_eq!(column(amp_row, "the AMP"), column(enter_row, "send"));

    // A narrow screen puts each description under its command, still whole words.
    for row in HelpBlock::new().render(30) {
        let row = rpi_tui::strip_ansi(&row);
        assert!(rpi_tui::visible_width(&row) <= 30, "{row:?}");
        for word in row.split_whitespace() {
            assert!(vocab.contains(word), "{word:?}: {row:?}");
        }
    }

    snapshot("help-80", &rows);
}

/// With mouse capture off (`[tui] mouse = false`, #2185) plain drag selects: /help names no
/// modifier and no terminal (#2210). On, it keeps the Fn/Option row.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn help_keys_follow_mouse_capture() {
    let _lock = LOCK.lock().await;
    let mut rig = rig().await;
    rig.shell.set_mouse_capture(false);
    rig.shell.submit("/help").await;
    let screen = rig.shell.view().render_plain(W, 80);
    assert!(
        screen.iter().any(|l| {
            let l = l.trim();
            l.starts_with("drag ") && l.ends_with("select text")
        }),
        "{}",
        screen.join("\n")
    );
    for gone in ["Fn/⌥-drag", "Terminal.app", "iTerm2", "wheel"] {
        assert!(
            !screen.iter().any(|l| l.contains(gone)),
            "{gone}\n{}",
            screen.join("\n")
        );
    }

    let rows = |on: bool| -> Vec<String> {
        HelpBlock::new()
            .mouse_capture(on)
            .render(HELP_80)
            .iter()
            .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
            .collect()
    };
    let (on, off) = (rows(true), rows(false));
    assert_eq!(
        on,
        HelpBlock::new()
            .render(HELP_80)
            .iter()
            .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
            .collect::<Vec<_>>()
    );
    assert!(on.iter().any(|r| r.contains("Fn/⌥-drag")), "{on:?}");
    assert!(!off.iter().any(|r| r.contains("Fn/⌥-drag")), "{off:?}");
    assert_eq!(keys(true), KEYS.to_vec());
    assert_eq!(keys(false).last(), Some(&DRAG_KEY));
    // #2347: clicking a worker needs mouse reporting, so its row goes too.
    assert_eq!(keys(false).len(), KEYS.len() - 1);
    assert!(keys(true).iter().any(|(k, _)| *k == "click"));
    assert!(!keys(false).iter().any(|(k, _)| *k == "click"));
    snapshot("help-80", &on);
    snapshot("help-80-mouse-off", &off);
}

fn snapshot(name: &str, rows: &[String]) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("{name}.txt"));
    let got = rows.join("\n") + "\n";
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::write(&path, &got).unwrap();
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

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_listed_command_is_one_the_shell_runs() {
    let mut rig = rig().await;
    for c in COMMANDS {
        let names = std::iter::once(c.name).chain(c.aliases.iter().copied());
        for name in names {
            let outcome = rig.shell.submit(&format!("/{name}")).await;
            if c.name == "quit" {
                assert_eq!(outcome, Submit::Quit, "/{name}");
                continue;
            }
            assert_eq!(outcome, Submit::Continue, "/{name}");
            assert!(
                !rig.screen().contains(&format!("unknown command: /{name}")),
                "/{name} is listed but unknown"
            );
        }
    }
}
