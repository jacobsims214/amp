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
//! - [`theme`]: the AMP theme (M3-1 #2103): our JSON theme format → rpi-tui `Theme` (Sharp
//!   borders) + [`theme::AmpPalette`] (ticket states, epic colours, brand bands); `amp-dark` and
//!   `amp-light` embedded, truecolor or 256 colours, dark/light from config or OSC 11.
//!
//! M3 hooks: [`view::ChatView::header_slot`] and [`view::ChatView::amp_status_slot`] are empty
//! containers for the AMP header and status line. [`install_theme`] installs the AMP theme; call
//! it before the shell is built. Components read [`theme::palette`] for AMP colours.

pub mod shell;
pub mod terminal;
pub mod theme;
pub mod view;

pub use shell::{HELP, ModelChoice, RunDone, Shell, ShellInfo, Submit};
pub use theme::{AmpPalette, AmpTheme, ColorDepth, ThemeSettings, Variant};
pub use view::{ChatView, FooterInfo};

/// Installs `theme` for every rpi-tui component (`global_theme_manager().set`, which also
/// invalidates cached renders) and as the current [`theme::palette`]. Call it before building
/// the shell.
pub fn install_theme(theme: &AmpTheme) {
    theme::install(theme);
}
