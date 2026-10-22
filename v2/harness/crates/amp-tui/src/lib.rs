//! AMP chat shell on rpi-tui (D10).
//!
//! M2-7 (#2097) ships the plain shell, modelled on rpi-cli's `tui_shell.rs`: pi's transcript,
//! editor and two-line footer (usage, model • thinking), the `/model`, `/usage`, `/help` and
//! `/quit` commands, and rpi-tui's default theme.
//!
//! - [`view::ChatView`]: the screen. Components are laid out header slot → transcript → status
//!   slot → editor → AMP status line slot → footer, and updated from rpi agent events. It renders
//!   at any size without a terminal ([`view::ChatView::render_plain`]).
//! - [`shell::Shell`]: the controller. It handles prompts (`prompt_text` on one harness, so the
//!   conversation carries on), slash commands and run outcomes, with no terminal involved.
//! - [`terminal::run`]: the full-screen terminal loop, which always restores the terminal.
//!
//! M3 hooks: [`view::ChatView::header_slot`] and [`view::ChatView::amp_status_slot`] are empty
//! containers for the AMP header and status line. [`install_theme`] is where the AMP theme
//! goes in, before the shell is built.

pub mod shell;
pub mod terminal;
pub mod view;

pub use shell::{HELP, ModelChoice, RunDone, Shell, ShellInfo, Submit};
pub use view::{ChatView, FooterInfo};

/// Installs `theme` for every rpi-tui component. Call it before building the shell. M2 keeps
/// rpi-tui's default (dark) theme; M3-1 passes the AMP theme loaded from our JSON format.
pub fn install_theme(theme: rpi_tui::Theme) {
    rpi_tui::global_theme_manager().set(theme);
}
