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
//! - [`chrome`]: the AMP header (brand bands, instance/user, org › project, MCP health; the current
//!   ticket) and the AMP status line (questions, workers, plan approval, board counts, run
//!   reported), M3-3 #2106. They sit in [`view::ChatView::header_slot`] and
//!   [`view::ChatView::amp_status_slot`] and show [`view::ChatView::chrome`]'s inputs (the
//!   metadata poller's snapshot); without a metadata source they render nothing.
//!
//! - [`overlay`] and [`amp_render`] (M3-4 #2108): the AMP slash commands' panels (`/amp`,
//!   `/ticket`, `/workers`, `/questions`, `/plan`, `/project`; `/open` opens the browser), `amp_*`
//!   tool calls titled `AMP · complete_task #2078`, and the transcript marker when the current
//!   ticket changes.
//!
//! [`install_theme`] installs the AMP theme; call it before the shell is built. Components read
//! [`theme::palette`] for AMP colours.

pub mod amp_render;
pub mod chrome;
pub mod overlay;
pub mod shell;
pub mod terminal;
pub mod theme;
pub mod view;

pub use chrome::{Chrome, ChromeData, ReportState};
pub use overlay::{Panel, PanelKind, PanelState, Stage, Target};
pub use shell::{
    ConnectFuture, ConnectReply, HELP, ModelChoice, Opener, ProviderConnect, RunDone, Shell,
    ShellInfo, Submit,
};
pub use theme::{AmpPalette, AmpTheme, ColorDepth, ThemeSettings, Variant};
pub use view::{ChatView, FooterInfo};

/// Installs `theme` for every rpi-tui component (`global_theme_manager().set`, which also
/// invalidates cached renders) and as the current [`theme::palette`]. Call it before building
/// the shell.
pub fn install_theme(theme: &AmpTheme) {
    theme::install(theme);
}
