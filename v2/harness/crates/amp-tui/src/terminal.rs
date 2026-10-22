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

use crossterm::event::{Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers, MouseEventKind};
use rpi_agent::AgentEvent;
use rpi_tui::{ProcessTerminal, TUI, Terminal, TerminalInfo, TuiAltScreen, TuiStopOptions};
use tokio::sync::mpsc::{UnboundedReceiver, unbounded_channel};

use crate::scroll::WHEEL_LINES;
use crate::shell::{CtrlC, CtrlD, QuitOutcome, RunDone, Shell, Submit};

/// Escape sequences that undo everything the TUI turns on: mouse tracking, bracketed paste,
/// the kitty keyboard protocol, a hidden cursor and the alternate screen.
const RESTORE_SEQ: &str =
    "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[<u\x1b[?25h\x1b[?1049l";

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
    inner: ProcessTerminal,
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
    }
    fn exit_raw_mode(&self) {
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
    }
    fn stop(&self) {
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
    let tui = Arc::new(TuiAltScreen::new(
        Box::new(MouseGate {
            inner: ProcessTerminal::new(),
            mouse: shell.mouse_capture(),
        }),
        false,
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
                        if input_tx.send(event).is_err() {
                            break;
                        }
                    }
                    Err(_) => break,
                }
            }
        })
    };

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
                tui.request_render(false);
            }
            Some(result) = done.recv() => {
                while let Ok(event) = events.try_recv() {
                    shell.handle_event(&event);
                }
                shell.finish_run(result);
                tui.request_render(false);
            }
            maybe_input = input_rx.recv() => {
                let Some(input) = maybe_input else { break };
                match input {
                    Event::Resize(..) => tui.note_resize(),
                    Event::Mouse(mouse) => {
                        if tui.handle_scroll_to_end_indicator_mouse_event(&mouse) {
                            continue;
                        }
                        match mouse.kind {
                            MouseEventKind::ScrollUp => shell.view().scroll().scroll_by(-WHEEL_LINES),
                            MouseEventKind::ScrollDown => shell.view().scroll().scroll_by(WHEEL_LINES),
                            _ => {}
                        }
                        tui.request_render(false);
                    }
                    Event::Key(key) if key.kind != KeyEventKind::Release => {
                        let editor = shell.view().editor().clone();
                        let editor_empty = editor.get_text().is_empty();
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
                        } else if is_ctrl(&key, 't') {
                            // Expand or collapse the last thinking (#2177).
                            shell.toggle_thinking();
                        } else if shell.key(&key, editor_empty).await {
                            // An open AMP panel took it (select, answer, open, close), or Esc
                            // stopped the run (Esc that closes a panel mid-run only hints).
                        } else {
                            editor.handle_key(key);
                        }
                        tui.request_render(false);
                    }
                    Event::Paste(text) => {
                        // Into the hidden `/connect` key when one is being typed.
                        shell.paste(&text);
                        tui.request_render(false);
                    }
                    _ => {}
                }
            }
            Some(text) = submit_rx.recv() => {
                let action = shell.submit(&text).await;
                shell.view().editor().clear();
                tui.request_render(false);
                if action == Submit::Quit {
                    break;
                }
            }
            _ = signals.recv() => break,
            _ = ticker.tick() => {
                tui.ensure_size();
                let retrying = shell.view().tick();
                // Finished AMP command calls, the ticket marker and the footer's #ticket.
                let amp_changed = shell.poll();
                let chrome = shell.view().chrome().fingerprint();
                let chrome_changed = chrome != chrome_seen;
                chrome_seen = chrome;
                if shell.is_running() || retrying || chrome_changed || amp_changed {
                    tui.request_render(false);
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
    }
}
