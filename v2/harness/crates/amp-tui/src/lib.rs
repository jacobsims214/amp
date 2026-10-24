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
//!   tool calls titled `complete_task #2078` with a one-line human summary (#2183), and the
//!   transcript marker when the current ticket changes.
//!
//! - [`transcript`] and [`wrap`] (#2176): the transcript's components, word-wrapped (rpi-tui
//!   0.8.0's own break lines mid-word).
//!
//! - [`slash`] (#2182): the slash commands' one table, the `/` completion menu above the editor,
//!   and `/help` as a grouped two-column list.
//!
//! - [`scroll`] (#2185): PageUp/PageDown, Ctrl+U/Ctrl+D and End scroll the transcript, and the
//!   "↓ new output" pill shows while it is scrolled up and output arrives below.
//!
//! - [`history`] (#2348): ↑↓ in the editor recall the prompts sent earlier in the chat.
//!
//! - [`footer`] (#2186): the footer in place of pi's: `~`-relative cwd shortened from the left,
//!   branch, role · #ticket, `$0.004 · ctx 4%` and model • thinking; one row below 100 columns.
//!
//! - [`inset`] (#2251): `[tui] padding` columns of space left and right of every area of the
//!   chat ([`inset::Inset`], one shared [`inset::Padding`]), shrinking on narrow terminals, and
//!   a blank row above and below it ([`inset::Gap`], #2344).
//!
//! [`install_theme`] installs the AMP theme; call it before the shell is built. Components read
//! [`theme::palette`] for AMP colours.

pub mod amp_render;
pub mod chrome;
pub mod density;
pub mod footer;
pub mod history;
pub mod inset;
pub mod overlay;
pub mod scroll;
pub mod shell;
pub mod slash;
pub mod table;
pub mod terminal;
pub mod theme;
pub mod transcript;
pub mod view;
pub mod workers;
pub mod wrap;

pub use chrome::{Chrome, ChromeData, ReportState};
pub use density::Density;
pub use history::{HISTORY_LIMIT, PromptHistory, Recall};
pub use inset::{
    DEFAULT_PADDING, Gap, Inset, MAX_PADDING, MIN_INNER, NARROW, Padding, SHORT, VERTICAL,
};
pub use overlay::{
    Cell, InfoPanel, InfoRow, ModelRow, Panel, PanelKey, PanelKind, PanelState, Stage, Target, Tone,
};
pub use scroll::{NEW_OUTPUT, TranscriptScroll};
pub use shell::{
    CTRL_D_SETTLE, ConfigControl, ConfigFuture, ConfigReply, ConnectFuture, ConnectReply, CtrlC,
    CtrlD, ESC_HINT, ESC_HINT_FOR, ModelChoice, Notifier, Opener, ProviderConnect, QUIT_HINT,
    QUIT_HINT_FOR, ReloadControl, ReloadFuture, ReloadReply, RunDone, STOPPED, ServerControl,
    ServerSwitched, Shell, ShellInfo, Submit,
};
pub use slash::{
    COMMANDS, CommandMenu, CommandSpec, DRAG_KEY, Group, HelpBlock, KEYS, MenuKey, keys,
};
pub use theme::{AmpPalette, AmpTheme, ColorDepth, ThemeSettings, Variant};
pub use transcript::ThinkingDisplay;
pub use view::{ChatView, FooterInfo, PLACEHOLDER, SessionParts};
pub use workers::{SessionView, stop_question};

/// Installs `theme` for every rpi-tui component (`global_theme_manager().set`, which also
/// invalidates cached renders) and as the current [`theme::palette`]. Call it before building
/// the shell.
pub fn install_theme(theme: &AmpTheme) {
    theme::install(theme);
}
