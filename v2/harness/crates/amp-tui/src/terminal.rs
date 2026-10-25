//! The terminal loop: [`Shell`] on rpi-tui's full-screen [`TuiAltScreen`], modelled on
//! rpi-cli's `tui_shell.rs` (readerless TUI, our own crossterm reader thread, a `select!` over
//! input, agent events, finished runs and a repaint tick).
//!
//! The terminal is always given back: [`TerminalGuard`] stops the TUI (leave the alternate screen,
//! raw mode off, cursor, mouse and bracketed paste restored) when the loop returns or unwinds, and
//! a panic hook does the same before the panic message prints. Ctrl-C in raw mode is a key, not
//! a signal: it aborts a running prompt, else clears the editor, else shows "Press Ctrl+C again to
//! quit", and a second Ctrl+C within 1.5 s quits ([`Shell::ctrl_c`]). Ctrl-D on the empty editor,
//! `/quit`, SIGINT, SIGTERM and SIGHUP quit at once.
//!
//! Scrolling (#2185): the wheel, PageUp/PageDown, Ctrl+U/Ctrl+D and End move the transcript
//! ([`Shell::scroll_key`], [`Shell::ctrl_d`]); while it is scrolled up and output arrives, rpi-tui's
//! jump-to-end indicator shows [`crate::NEW_OUTPUT`] (clicking it jumps to the end). Mouse
//! reporting is on unless [`Shell::mouse_capture`] is off (`[tui] mouse = false`), so the
//! terminal's own click-drag selection needs a modifier while it is on: Fn-drag in Terminal.app,
//! Option-drag in iTerm2 (see the README).
//!
//! The cursor (#2344): while the input has focus ([`Shell::input_focused`]) the terminal's own
//! cursor shows at the insertion point as a blinking bar (`SetCursorStyle::BlinkingBar` on
//! entering raw mode); while a panel or prompt has the keys it is hidden. A left click into the
//! editor box focuses it and moves the insertion point there ([`Shell::click`]). Leaving raw mode
//! (quit, suspend), the guard and the panic hook put back the terminal's default cursor shape
//! and show it.
//!
//! Workers (#2347): a left click on a worker chip above the editor, a `/workers` row, a worker
//! notice or dispatch line, or a `#ticket` with a worker session opens that worker's live session
//! ([`Shell::click`]); Ctrl+W (with an empty editor) gives the worker strip the keys
//! ([`Shell::toggle_workers_focus`]).
//!
//! Quitting with a prompt or workers still running goes through [`Shell::quit`]: the workers are
//! cancelled and the screen says it is waiting for their run reports (bounded; a second Ctrl-C or
//! signal skips the wait). Tickets still unreported when the TUI exits are printed to stderr.

use std::{
    io::Write,
    sync::{
        Arc, Once,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use crossterm::cursor::SetCursorStyle;
use crossterm::event::{
    Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers, MouseButton, MouseEventKind,
};
use rpi_agent::AgentEvent;
use rpi_tui::{ProcessTerminal, TUI, Terminal, TerminalInfo, TuiAltScreen, TuiStopOptions};
use tokio::sync::mpsc::{UnboundedReceiver, unbounded_channel};

use crate::newline::NewlineMode;
use crate::scroll::WHEEL_LINES;
use crate::shell::{CtrlC, CtrlD, QuitOutcome, RunDone, Shell, Submit};

/// Escape sequences that undo everything the TUI turns on: mouse tracking, bracketed paste,
/// the kitty keyboard protocol, the bar cursor (back to the terminal's default shape), a hidden
/// cursor and the alternate screen.
const RESTORE_SEQ: &str =
    "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[<u\x1b[0 q\x1b[?25h\x1b[?1049l";

/// A crossterm command as the escape sequence it writes.
fn ansi(command: impl crossterm::Command) -> String {
    let mut out = String::new();
    let _ = command.write_ansi(&mut out);
    out
}

/// The input's cursor: a blinking bar (#2344).
pub fn cursor_bar() -> String {
    ansi(SetCursorStyle::BlinkingBar)
}

/// The terminal's own cursor shape, put back on the way out.
pub fn cursor_default() -> String {
    ansi(SetCursorStyle::DefaultUserShape)
}

/// Puts the terminal back the way the shell found it, best effort, without the TUI object.
pub fn restore_terminal() {
    let _ = crossterm::terminal::disable_raw_mode();
    let mut out = std::io::stdout();
    let _ = out.write_all(RESTORE_SEQ.as_bytes());
    let _ = out.flush();
}

/// Installs (once) a panic hook that restores the terminal before the default hook prints.
fn install_panic_hook() {
    static ONCE: Once = Once::new();
    ONCE.call_once(|| {
        let previous = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            if ACTIVE.load(Ordering::SeqCst) {
                restore_terminal();
            }
            previous(info);
        }));
    });
}

/// Whether a TUI currently owns the terminal (for the panic hook).
static ACTIVE: AtomicBool = AtomicBool::new(false);

/// Stops the TUI exactly once, on every way out of [`run`].
struct TerminalGuard {
    tui: Arc<TuiAltScreen>,
    stopped: bool,
}

impl TerminalGuard {
    fn stop(&mut self) {
        if !self.stopped {
            self.stopped = true;
            self.tui.stop(TuiStopOptions::default());
            let _ = crossterm::terminal::disable_raw_mode();
            ACTIVE.store(false, Ordering::SeqCst);
        }
    }
}

impl Drop for TerminalGuard {
    fn drop(&mut self) {
        self.stop();
    }
}

/// The process terminal, with mouse reporting kept off when `mouse` is false (`[tui] mouse`):
/// rpi-tui turns it on whenever it enters the alternate screen or raw mode.
struct MouseGate {
    /// Shared with [`run`], which reads whether the kitty keyboard protocol was negotiated.
    inner: Arc<ProcessTerminal>,
    mouse: bool,
}

impl Terminal for MouseGate {
    fn info(&self) -> TerminalInfo {
        let mut info = self.inner.info();
        info.supports_mouse &= self.mouse;
        info
    }
    fn write(&self, data: &str) {
        self.inner.write(data)
    }
    fn hide_cursor(&self) {
        self.inner.hide_cursor()
    }
    fn show_cursor(&self) {
        self.inner.show_cursor()
    }
    fn move_cursor(&self, row: usize, col: usize) {
        self.inner.move_cursor(row, col)
    }
    fn clear_screen(&self) {
        self.inner.clear_screen()
    }
    fn set_title(&self, title: &str) {
        self.inner.set_title(title)
    }
    fn enable_mouse(&self) {
        if self.mouse {
            self.inner.enable_mouse()
        } else {
            self.inner.disable_mouse()
        }
    }
    fn disable_mouse(&self) {
        self.inner.disable_mouse()
    }
    fn enter_raw_mode(&self) {
        self.inner.enter_raw_mode();
        if !self.mouse {
            self.inner.disable_mouse();
        }
        // #2344: the input's cursor is a blinking bar while the chat owns the terminal.
        self.inner.write(&cursor_bar());
    }
    fn exit_raw_mode(&self) {
        // Suspend and quit: the shell gets its own cursor shape back, showing.
        self.inner.write(&cursor_default());
        self.inner.show_cursor();
        self.inner.exit_raw_mode()
    }
    fn refresh_size(&self) {
        self.inner.refresh_size()
    }
    fn start(
        &self,
        on_input: Box<dyn Fn(rpi_tui::terminal::InputEvent) + Send + Sync>,
        on_resize: Box<dyn Fn() + Send + Sync>,
    ) {
        self.inner.start(on_input, on_resize);
        if !self.mouse {
            self.inner.disable_mouse();
        }
        self.inner.write(&cursor_bar());
    }
    fn stop(&self) {
        self.inner.write(&cursor_default());
        self.inner.show_cursor();
        self.inner.stop()
    }
    fn is_tty(&self) -> bool {
        self.inner.is_tty()
    }
    fn set_progress(&self, active: bool) {
        self.inner.set_progress(active)
    }
    fn flush(&self) {
        self.inner.flush()
    }
}

/// Shows or hides the cursor for who has the keys, then asks for a frame (#2344).
fn repaint(shell: &mut Shell, tui: &TuiAltScreen) {
    shell.sync_focus();
    fit_height(shell);
    tui.request_render(false);
}

/// The blank rows above and below the chat for the terminal's height (#2344). Returns whether
/// they changed.
fn fit_height(shell: &Shell) -> bool {
    let Ok((_, rows)) = crossterm::terminal::size() else {
        return false;
    };
    let before = shell.view().vertical_padding();
    shell.view().set_height(usize::from(rows));
    before != shell.view().vertical_padding()
}

/// A key as the reader hands it on (#2349): rpi-tui's normalization for this protocol (a local
/// Terminal.app's Shift+Enter, legacy iTerm2's Option+arrows; `kitty`: whether the kitty keyboard
/// protocol is on), then Alt+Enter as Shift+Enter ([`crate::newline::normalize`]).
pub fn read_key(key: KeyEvent, kitty: bool) -> KeyEvent {
    crate::newline::normalize(rpi_tui::terminal::normalize_key_event_with_protocol(
        key, kitty,
    ))
}

fn is_ctrl(key: &KeyEvent, ch: char) -> bool {
    key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char(ch)
}

/// Opens the chat on the terminal and runs until the user quits.
pub async fn run(
    mut shell: Shell,
    mut events: UnboundedReceiver<AgentEvent>,
    mut done: UnboundedReceiver<RunDone>,
) -> Result<(), String> {
    install_panic_hook();
    let terminal = Arc::new(ProcessTerminal::new());
    let tui = Arc::new(TuiAltScreen::new(
        Box::new(MouseGate {
            inner: Arc::clone(&terminal),
            mouse: shell.mouse_capture(),
        }),
        // #2344: the terminal's cursor at the editor's insertion point (rpi-tui's cursor marker).
        true,
        None,
    ));
    // "↓ new output" while the transcript is scrolled up and output arrives (#2185).
    let scroll = shell.view().scroll().clone();
    tui.set_scroll_to_end_indicator(Some(Arc::new(move || scroll.pill_label())));
    tui.set_title(&format!("amp-harness · {}", shell.info().role));
    tui.set_layout_root(Some(shell.view().root()));
    tui.set_focus(Some(shell.view().editor().clone()));
    ACTIVE.store(true, Ordering::SeqCst);
    let mut guard = TerminalGuard {
        tui: Arc::clone(&tui),
        stopped: false,
    };
    tui.start_render_scheduler();
    tui.start_readerless();
    // #2349: entering raw mode negotiated the kitty keyboard protocol (or not). Our reader does
    // what rpi-tui's own reader does with each key (Terminal.app's Shift+Enter, iTerm2's
    // Option+arrows), and Alt+Enter becomes a new line.
    let kitty = terminal.info().supports_kitty_keyboard;
    shell.set_newline_mode(NewlineMode::detect(kitty));

    let (submit_tx, mut submit_rx) = unbounded_channel::<String>();
    shell.view().editor().on_submit(Arc::new(move |text: &str| {
        let _ = submit_tx.send(text.to_owned());
    }));

    // One reader of stdin (start_readerless leaves input to us).
    let (input_tx, mut input_rx) = unbounded_channel::<Event>();
    let stop_reader = Arc::new(AtomicBool::new(false));
    let reader = {
        let stop = Arc::clone(&stop_reader);
        tokio::task::spawn_blocking(move || {
            while !stop.load(Ordering::Relaxed) {
                match crossterm::event::poll(Duration::from_millis(50)) {
                    Ok(true) => {}
                    Ok(false) => continue,
                    Err(_) => break,
                }
                match crossterm::event::read() {
                    Ok(event) => {
                        let event = match event {
                            Event::Key(key) => Event::Key(read_key(key, kitty)),
                            other => other,
                        };
                        if input_tx.send(event).is_err() {
                            break;
                        }
                    }
                    Err(_) => break,
                }
            }
        })
    };

    shell.sync_focus();
    fit_height(&shell);
    tui.request_render(true);
    let mut signals = Signals::new();
    let outcome = event_loop(
        &mut shell,
        &tui,
        &mut events,
        &mut done,
        &mut input_rx,
        &mut submit_rx,
        &mut signals,
    )
    .await;

    // Quitting mid-run: cancel the workers, abort the prompt, and wait (bounded) for the workers'
    // run reports. Ctrl-C or another signal skips the wait.
    let skip = async {
        loop {
            tokio::select! {
                input = input_rx.recv() => match input {
                    Some(Event::Key(key)) if key.kind != KeyEventKind::Release && is_ctrl(&key, 'c') => break,
                    Some(_) => {}
                    None => std::future::pending::<()>().await,
                },
                () = signals.recv() => break,
            }
        }
    };
    let quit = shell
        .quit(&mut events, &mut done, skip, |_| {
            tui.ensure_size();
            tui.request_render(false);
        })
        .await;
    let unreported = match quit {
        QuitOutcome::Skipped | QuitOutcome::TimedOut => shell.live_tickets(),
        QuitOutcome::Idle | QuitOutcome::Settled => Vec::new(),
    };
    stop_reader.store(true, Ordering::Relaxed);
    let _ = reader.await;
    guard.stop();
    if !unreported.is_empty() {
        let ids: Vec<String> = unreported.iter().map(|id| format!("#{id}")).collect();
        eprintln!(
            "amp-harness: quit before the run of {} was reported to AMP",
            ids.join(", ")
        );
    }
    // #2204: what became of the tickets whose workers the quit abandoned.
    for (id, result) in shell.released() {
        match result {
            Ok(()) => eprintln!("amp-harness: #{id} was returned to ready in AMP"),
            Err(e) => eprintln!(
                "amp-harness: #{id} is still in progress in AMP (could not return it to ready: {e})"
            ),
        }
    }
    outcome
}

async fn event_loop(
    shell: &mut Shell,
    tui: &Arc<TuiAltScreen>,
    events: &mut UnboundedReceiver<AgentEvent>,
    done: &mut UnboundedReceiver<RunDone>,
    input_rx: &mut UnboundedReceiver<Event>,
    submit_rx: &mut UnboundedReceiver<String>,
    signals: &mut Signals,
) -> Result<(), String> {
    let mut ticker = tokio::time::interval(Duration::from_millis(120));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    // The AMP header and status line repaint when the metadata, workers or reports change.
    let mut chrome_seen = shell.view().chrome().fingerprint();
    loop {
        tokio::select! {
            Some(event) = events.recv() => {
                shell.handle_event(&event);
                repaint(shell, tui);
            }
            Some(result) = done.recv() => {
                while let Ok(event) = events.try_recv() {
                    shell.handle_event(&event);
                }
                shell.finish_run(result);
                repaint(shell, tui);
            }
            maybe_input = input_rx.recv() => {
                let Some(input) = maybe_input else { break };
                match input {
                    Event::Resize(cols, rows) => {
                        // #2376: `/remote` sizes its QR code by the screen.
                        shell.set_screen(usize::from(cols), usize::from(rows));
                        shell.view().set_height(usize::from(rows));
                        tui.note_resize();
                    }
                    Event::Mouse(mouse) => {
                        if tui.handle_scroll_to_end_indicator_mouse_event(&mouse) {
                            continue;
                        }
                        match mouse.kind {
                            MouseEventKind::ScrollUp => shell.view().scroll().scroll_by(-WHEEL_LINES),
                            MouseEventKind::ScrollDown => shell.view().scroll().scroll_by(WHEEL_LINES),
                            MouseEventKind::Down(MouseButton::Left) => {
                                // #2344: a click into the editor focuses it, cursor where it landed;
                                // #2347: a click on a worker (or its #ticket) opens its session.
                                // #2345: one on a thinking header opens or closes it.
                                if let Ok((w, h)) = crossterm::terminal::size() {
                                    shell.click(
                                        usize::from(w),
                                        usize::from(h),
                                        usize::from(mouse.row),
                                        usize::from(mouse.column),
                                    );
                                }
                            }
                            _ => {}
                        }
                        repaint(shell, tui);
                    }
                    Event::Key(key) if key.kind != KeyEventKind::Release => {
                        let editor_empty = shell.view().editor().get_text().is_empty();
                        if !is_ctrl(&key, 'c') {
                            // The quit window is two Ctrl+Cs in a row.
                            shell.clear_quit_hint();
                        }
                        if shell.secret_key(&key) {
                            // A `/connect` key is being typed: it never reaches the editor.
                        } else if is_ctrl(&key, 'c') {
                            // Stop the reply, else clear the editor, else "Press Ctrl+C again
                            // to quit", and the second one quits (#2184).
                            if shell.ctrl_c(Instant::now()).await == CtrlC::Quit {
                                break;
                            }
                        } else if is_ctrl(&key, 'd') && editor_empty {
                            // Half a page down while scrolled up; at the latest output it stops
                            // the reply or quits, as before (#2185).
                            if shell.ctrl_d(Instant::now()).await == CtrlD::Quit {
                                break;
                            }
                        } else if is_ctrl(&key, 'w')
                            && (editor_empty
                                || shell.viewing().is_some()
                                || shell.view().strip().is_focused())
                            && shell.toggle_workers_focus()
                        {
                            // #2347: the workers above the editor take the keys (←/→, Enter).
                        } else if is_ctrl(&key, 't') {
                            // Expand or collapse the thinking in sight, else the last (#2177,
                            // #2345).
                            if let Ok((w, h)) = crossterm::terminal::size() {
                                shell.set_screen(usize::from(w), usize::from(h));
                            }
                            shell.toggle_thinking();
                        } else if is_ctrl(&key, 'o') {
                            // Show or hide the tool calls' details (#2183).
                            shell.toggle_tools();
                        } else if shell.key(&key, editor_empty).await {
                            // An open AMP panel took it (select, answer, open, close), or Esc
                            // stopped the run (Esc that closes a panel mid-run only hints).
                        } else {
                            // `\` Enter starts a new line; the rest is the editor's (#2349).
                            shell.editor_key(key);
                        }
                        repaint(shell, tui);
                    }
                    Event::Paste(text) => {
                        // Into the hidden `/connect` key when one is being typed.
                        shell.paste(&text);
                        repaint(shell, tui);
                    }
                    _ => {}
                }
            }
            Some(text) = submit_rx.recv() => {
                let action = shell.submit(&text).await;
                shell.view().editor().clear();
                repaint(shell, tui);
                if action == Submit::Quit {
                    break;
                }
            }
            _ = signals.recv() => break,
            _ = ticker.tick() => {
                tui.ensure_size();
                let resized = fit_height(shell);
                let retrying = shell.view().tick();
                // Finished AMP command calls, the ticket marker and the footer's #ticket.
                let amp_changed = shell.poll();
                let chrome = shell.view().chrome().fingerprint();
                let chrome_changed = chrome != chrome_seen;
                chrome_seen = chrome;
                let focused = shell.view().input_focused();
                shell.sync_focus();
                let focus_changed = focused != shell.view().input_focused();
                if shell.is_running() || retrying || chrome_changed || amp_changed || focus_changed || resized {
                    repaint(shell, tui);
                }
            }
        }
    }
    Ok(())
}

/// SIGINT / SIGTERM / SIGHUP: quit (the guard restores the terminal).
struct Signals {
    #[cfg(unix)]
    term: Option<tokio::signal::unix::Signal>,
    #[cfg(unix)]
    hup: Option<tokio::signal::unix::Signal>,
}

impl Signals {
    fn new() -> Self {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{SignalKind, signal};
            Self {
                term: signal(SignalKind::terminate()).ok(),
                hup: signal(SignalKind::hangup()).ok(),
            }
        }
        #[cfg(not(unix))]
        Self {}
    }

    async fn recv(&mut self) {
        #[cfg(unix)]
        {
            let term = async {
                match self.term.as_mut() {
                    Some(s) => {
                        s.recv().await;
                    }
                    None => std::future::pending::<()>().await,
                }
            };
            let hup = async {
                match self.hup.as_mut() {
                    Some(s) => {
                        s.recv().await;
                    }
                    None => std::future::pending::<()>().await,
                }
            };
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {}
                _ = term => {}
                _ = hup => {}
            }
        }
        #[cfg(not(unix))]
        {
            let _ = tokio::signal::ctrl_c().await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ctrl_detection_is_specific() {
        assert!(is_ctrl(
            &KeyEvent::new(KeyCode::Char('c'), KeyModifiers::CONTROL),
            'c'
        ));
        assert!(!is_ctrl(
            &KeyEvent::new(KeyCode::Char('c'), KeyModifiers::NONE),
            'c'
        ));
    }

    #[test]
    fn the_restore_sequence_leaves_the_alternate_screen_and_shows_the_cursor() {
        assert!(RESTORE_SEQ.contains("\x1b[?1049l"));
        assert!(RESTORE_SEQ.contains("\x1b[?25h"));
        assert!(RESTORE_SEQ.contains("\x1b[?2004l"));
        // #2344: and the terminal's own cursor shape.
        assert!(RESTORE_SEQ.contains(&cursor_default()));
    }

    #[test]
    fn the_cursor_is_a_blinking_bar_and_goes_back_to_the_default_shape() {
        assert_eq!(cursor_bar(), "\x1b[5 q");
        assert_eq!(cursor_default(), "\x1b[0 q");
    }
}
