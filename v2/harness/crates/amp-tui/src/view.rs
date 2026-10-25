//! The chat screen: pi's components laid out as D10 describes, updated from rpi agent events.
//!
//! Top to bottom:
//! 1. **header slot**: the AMP header ([`crate::chrome`]: brand bands, instance/user,
//!    org › project, MCP health; the current ticket);
//! 2. the **transcript** in a primary scroll view (banner, user messages, assistant messages,
//!    tool calls, notices), built from rpi-tui's own components so it reads like pi;
//! 3. pi's **status slot** (the retry countdown);
//! 4. the **AMP panel slot** ([`crate::overlay`]): what `/amp`, `/ticket`, `/workers`,
//!    `/questions`, `/plan` and `/project` show, docked above the editor while open;
//! 5. the **`/` menu** ([`crate::slash`]): the commands matching what is typed after `/`;
//! 6. the **editor** (pi's, with the working spinner in its top border while a run is going, and
//!    the dim placeholder [`PLACEHOLDER`] while it is empty);
//! 7. **AMP status line slot**: [`crate::chrome`]'s status line (questions, workers, plan
//!    approval, board counts, artifacts in review, run reported);
//! 8. the **footer** ([`crate::footer`], #2186): cwd (branch) • session, `$0.004 · ctx 4%` and
//!    model • thinking; one row below [`crate::footer::FOLD_BELOW`] columns, two above (one at
//!    any width when compact, [`crate::density`]).
//!
//! Every area sits [`crate::inset::Padding`] columns in from the left and right edges
//! (`[tui] padding`, #2251; halved at 80 columns or fewer, #2344); the transcript's inset is
//! inside its scroll view. A blank row sits above the header and below the footer
//! ([`crate::inset::Gap`], #2344; none on terminals under 24 rows): call
//! [`ChatView::set_height`] with the terminal's height before a frame.
//!
//! The editor carries rpi-tui's cursor marker while the input has focus
//! ([`ChatView::set_input_focus`], #2344), which the terminal turns into its real cursor (a
//! blinking bar, [`crate::terminal`]); while a panel or prompt has the keys there is no marker
//! and the cursor is hidden. [`ChatView::editor_hit`] maps a click to a place in the text.
//!
//! Nothing here touches a terminal: [`ChatView::render_frame`] lays the screen out at any size,
//! which is what the render tests (and `amp-harness --snapshot`) use. The header and status line
//! render nothing until [`ChatView::chrome`] has a metadata source.
//!
//! Tool calls are one line each (#2183): `✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms`,
//! `amp_*` ones titled and summarised by [`crate::amp_render`]. Consecutive calls share one
//! [`ToolGroup`]; Ctrl+O shows every call's arguments and result ([`ChatView::toggle_tools`]).
//!
//! The transcript's entries are [`crate::transcript`]'s components, which look like pi's but
//! word-wrap (#2176): rpi-tui 0.8.0's own components break lines mid-word.

use std::{
    collections::HashMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use amp_core::events::{stop_reason_str, tool_result_text};
use amp_core::provider_error;

use crate::amp_render;
use crate::chrome::{Chrome, clean, shorten};
use crate::footer::Footer;
use crate::inset::{Gap, Inset, Padding};
use crate::overlay::Panel;
use crate::roster::{NoticeKind, Roster, WorkerNotice, WorkerStrip};
use crate::scroll::{TranscriptScroll, paint_pill};
use crate::slash::CommandMenu;
use crate::transcript::{
    AssistantBlock, Block, THINKING_HINT, THINKING_HINT_KEY, ThinkingDisplay, ToolBlock,
    ToolDisplay, ToolGroup, UserBlock, WrapText,
};
use rpi_agent::{AgentEvent, AgentMessage};
use rpi_ai::types::{AssistantMessage, Content, StopReason, Usage};
use rpi_tui::{
    CURSOR_MARKER, Component, Container, Editor, EditorOptions, EditorStyle, Focusable,
    Keybindings, ScrollView, StackChild, StackEntry, StatusIndicator, VStack, WorkingState,
    strip_ansi, theme,
};

/// The tools that hand a ticket to a worker: their line gets the worker's live row (#2347).
const WORKER_TOOLS: &[&str] = &["amp_run_ticket", "amp_dispatch_task"];

/// The dim hint in the empty editor (#2182).
pub const PLACEHOLDER: &str = "/ for commands";

/// The empty editor's hint where Shift+Enter sends like Enter (no kitty keyboard protocol,
/// #2349): it names the key that starts a new line. [`PLACEHOLDER`] when it doesn't fit.
pub const LEGACY_PLACEHOLDER: &str = "/ for commands · Ctrl+J new line";

/// What the footer shows about the session. The footer's own setters take these.
#[derive(Debug, Clone, Default)]
pub struct FooterInfo {
    /// The model id (the footer shows it right-aligned on line 2).
    pub model: String,
    /// `off`, `low`, `high`, ...
    pub thinking: String,
    /// The model's context window in tokens (0 = unknown).
    pub context_window: u64,
    /// Working directory (line 1).
    pub cwd: String,
    /// Git branch (line 1, in parentheses), when the cwd is in a repository.
    pub git_branch: Option<String>,
    /// Session name (line 1, after a bullet): the role for now; M3 puts the current ticket here.
    pub session_name: Option<String>,
}

/// What [`ChatView::show_session`] swaps in to show a worker's session (#2252): its
/// transcript (in the main scroll view), the bar above it, and the keys line in the editor's
/// place.
#[derive(Clone)]
pub struct SessionParts {
    pub transcript: Arc<dyn Component>,
    pub bar: Arc<dyn Component>,
    pub keys: Arc<dyn Component>,
}

/// The chat screen's components and transcript state.
pub struct ChatView {
    header: Arc<Container>,
    transcript: Arc<Container>,
    /// Holds the transcript, or a worker's while its session is shown (#2252).
    transcript_slot: Arc<Container>,
    /// The worker session's bar, under the header (empty: 0 lines).
    session_slot: Arc<Container>,
    /// Holds the editor, or the session's keys line.
    editor_slot: Arc<Container>,
    editor_view: Arc<dyn Component>,
    session_shown: std::sync::atomic::AtomicBool,
    status: Arc<Container>,
    editor: Arc<Editor>,
    amp_status: Arc<Container>,
    footer: Arc<Footer>,
    /// The AMP header and status line's inputs (M3-3).
    chrome: Chrome,
    /// The AMP command panel (M3-4), above the editor while open.
    panel: Panel,
    /// The `/` completion menu (#2182), right above the editor while a command is being typed.
    menu: CommandMenu,
    root: Arc<dyn Component>,
    /// The transcript's scroll position and the "↓ new output" pill (#2185).
    scroll: TranscriptScroll,
    /// The assistant message being streamed, if any.
    assistant: Option<Arc<AssistantBlock>>,
    /// Running tool calls by id.
    tools: HashMap<String, Arc<ToolBlock>>,
    /// The tool group the next call joins, while nothing visible has followed it (#2183).
    group: Mutex<Option<OpenGroup>>,
    /// Collapsed or expanded tool calls (Ctrl+O), shared by every call.
    tool_display: Arc<ToolDisplay>,
    /// The retry indicator in the status slot, if one is shown.
    retry: Option<Arc<StatusIndicator>>,
    /// How thinking shows (`[tui] show_thinking`, #2177).
    show_thinking: ThinkingDisplay,
    /// The last assistant message with thinking: what Ctrl+T expands when no thinking header is
    /// in sight.
    last_thinking: Option<Arc<AssistantBlock>>,
    /// Clicks reach the chat (`[tui] mouse`): the last thinking's hint names them (#2345).
    mouse: bool,
    /// The person asked the run to stop: its aborted (or failed) reply keeps what it streamed
    /// and shows no error, since the shell says "Stopped." once (#2181).
    stopping: bool,
    /// A reply ended in an error and the transcript shows it: the shell's "run failed" line
    /// doesn't repeat it (#2207). Taken by [`Self::take_error_shown`].
    error_shown: bool,
    /// What the empty editor says instead of [`PLACEHOLDER`] for a moment ("Press Ctrl+C again
    /// to quit", #2184).
    hint: Arc<Mutex<Option<String>>>,
    /// The empty editor's usual hint: [`PLACEHOLDER`], or [`LEGACY_PLACEHOLDER`] (#2349).
    placeholder: Arc<Mutex<&'static str>>,
    /// The left and right padding every area shares (#2251).
    padding: Padding,
    /// The editor's inset (what the layout places), to find it in a frame for clicks (#2344).
    editor_box: Arc<dyn Component>,
    /// Whether the input has the keys: the editor then shows the terminal cursor (#2344).
    input_focus: Arc<AtomicBool>,
    /// The blank rows above and below the screen (#2344).
    gap: Gap,
    /// What each worker is for and how it is doing (#2347), shared with the strip, the
    /// transcript's worker lines and the chrome.
    roster: Roster,
    /// The workers above the editor (#2347).
    strip: Arc<WorkerStrip>,
    /// While [`Self::apply_at`] applies an event: when it happened (`Some(None)`: not known).
    event_time: Option<Option<Instant>>,
}

impl Default for ChatView {
    fn default() -> Self {
        Self::new()
    }
}

impl ChatView {
    pub fn new() -> Self {
        let padding = Padding::default();
        let inset = |c: Arc<dyn Component>| -> Arc<dyn Component> {
            Arc::new(Inset::new(c, padding.clone()))
        };
        let chrome = Chrome::new();
        let roster = Roster::default();
        chrome.set_roster(roster.clone());
        let strip = Arc::new(WorkerStrip::new(roster.clone()));
        let header = Arc::new(Container::new());
        header.add_child(chrome.header());
        let transcript = Arc::new(Container::new());
        let transcript_slot = Arc::new(Container::new());
        transcript_slot.add_child(transcript.clone());
        let scroll = TranscriptScroll::new(Arc::new(ScrollView::primary(inset(
            transcript_slot.clone(),
        ))));
        let session_slot = Arc::new(Container::new());
        let status = Arc::new(Container::new());
        let panel = Panel::new(chrome.clone());
        let panel_slot = Arc::new(Container::new());
        panel_slot.add_child(panel.component());
        let editor = Arc::new(Editor::new(
            EditorOptions::default(),
            EditorStyle::default(),
            Arc::new(Keybindings::new()),
        ));
        editor.set_focused(true);
        let menu = CommandMenu::new(editor.clone());
        let menu_slot = Arc::new(Container::new());
        menu_slot.add_child(Arc::new(menu.clone()));
        let hint = Arc::new(Mutex::new(None));
        let placeholder = Arc::new(Mutex::new(PLACEHOLDER));
        let input_focus = Arc::new(AtomicBool::new(true));
        let editor_slot = Arc::new(Container::new());
        let editor_view: Arc<dyn Component> = Arc::new(EditorView {
            editor: editor.clone(),
            hint: Arc::clone(&hint),
            placeholder: Arc::clone(&placeholder),
            focus: Arc::clone(&input_focus),
        });
        editor_slot.add_child(editor_view.clone());
        let amp_status = Arc::new(Container::new());
        amp_status.add_child(chrome.status_line());
        let footer = Arc::new(Footer::new());
        let editor_box = inset(editor_slot.clone());
        let gap = Gap::default();

        let root = VStack::from_children(vec![
            StackChild::Entry(StackEntry::new(Arc::new(gap.clone())).shrink(0)),
            StackChild::Entry(StackEntry::new(inset(header.clone())).shrink(0)),
            StackChild::Entry(StackEntry::new(inset(session_slot.clone())).shrink(0)),
            StackChild::Entry(
                StackEntry::new(scroll.view().clone())
                    .basis(0)
                    .grow(1)
                    .shrink(1)
                    .min_size(1),
            ),
            StackChild::Entry(StackEntry::new(inset(status.clone()))),
            StackChild::Entry(StackEntry::new(inset(panel_slot)).shrink(1)),
            StackChild::Entry(StackEntry::new(inset(menu_slot)).shrink(1)),
            StackChild::Entry(StackEntry::new(inset(strip.clone())).shrink(1)),
            StackChild::Entry(StackEntry::new(editor_box.clone()).shrink(0).min_size(3)),
            StackChild::Entry(StackEntry::new(inset(amp_status.clone())).shrink(0)),
            StackChild::Entry(StackEntry::new(inset(footer.clone()))),
            StackChild::Entry(StackEntry::new(Arc::new(gap.clone())).shrink(0)),
        ]);
        Self {
            header,
            transcript,
            transcript_slot,
            session_slot,
            editor_slot,
            editor_view,
            session_shown: std::sync::atomic::AtomicBool::new(false),
            status,
            editor,
            amp_status,
            footer,
            chrome,
            panel,
            menu,
            root: Arc::new(root),
            scroll,
            assistant: None,
            tools: HashMap::new(),
            group: Mutex::new(None),
            tool_display: Arc::new(ToolDisplay::default()),
            retry: None,
            show_thinking: ThinkingDisplay::default(),
            last_thinking: None,
            mouse: true,
            stopping: false,
            error_shown: false,
            hint,
            placeholder,
            padding,
            editor_box,
            input_focus,
            gap,
            roster,
            strip,
            event_time: None,
        }
    }

    /// What each worker is for and how it is doing (#2347): the shell fills it on its tick.
    pub fn roster(&self) -> &Roster {
        &self.roster
    }

    /// The workers above the editor (#2347).
    pub fn strip(&self) -> &Arc<WorkerStrip> {
        &self.strip
    }

    /// Adds a `▶ worker started` / `■ worker finished` line for `task_id` (#2347).
    pub fn add_worker_notice(&self, task_id: i64, kind: NoticeKind) {
        self.add_to_transcript(Arc::new(WorkerNotice::new(
            task_id,
            kind,
            self.roster.clone(),
        )));
    }

    /// The terminal's height, before a frame is laid out (#2344): picks the blank rows above
    /// and below the screen. [`Self::render_frame`] calls it itself.
    pub fn set_height(&self, height: usize) {
        let rows = self.gap.rows();
        self.gap.set_height(height);
        if rows != self.gap.rows() {
            self.root.invalidate();
        }
    }

    /// The blank rows above and below the screen now.
    pub fn vertical_padding(&self) -> usize {
        self.gap.rows()
    }

    /// The screen laid out at `width` x `height`.
    fn layout(&self, width: usize, height: usize) -> rpi_tui::LayoutFrame {
        self.set_height(height);
        rpi_tui::render_layout_frame(self.root(), width, height)
    }

    /// Whether the input has the keys (#2344). While it does, the editor marks its insertion
    /// point and the terminal shows its cursor there; while a panel or prompt has them, the
    /// cursor is hidden.
    pub fn set_input_focus(&self, focused: bool) {
        self.input_focus.store(focused, Ordering::Relaxed);
    }

    pub fn input_focused(&self) -> bool {
        self.input_focus.load(Ordering::Relaxed)
    }

    /// Where the insertion point is on a `width` x `height` screen: `(row, column)`, 0-based, or
    /// `None` while the input doesn't have focus (no cursor shows). What the terminal's cursor
    /// is moved to.
    pub fn cursor_position(&self, width: usize, height: usize) -> Option<(usize, usize)> {
        let frame = self.layout(width, height);
        frame.lines.iter().enumerate().find_map(|(row, line)| {
            line.find(CURSOR_MARKER)
                .map(|at| (row, rpi_tui::visible_width(&line[..at])))
        })
    }

    /// A click at `(row, column)` (0-based) on a `width` x `height` screen (#2344): inside the
    /// editor box, the text position it lands on, as `(line, byte offset)` for
    /// [`Editor::set_cursor`]. A click on the box's borders goes to the nearest text row, one
    /// past a row's end to its end. `None` outside the box, or while a worker's session is shown
    /// in the editor's place.
    pub fn editor_hit(
        &self,
        width: usize,
        height: usize,
        row: usize,
        column: usize,
    ) -> Option<(usize, usize)> {
        if self.session_shown() {
            return None;
        }
        let frame = self.layout(width, height);
        let rect = find_box(&frame.root, &self.editor_box)?;
        if row < rect.y || row >= rect.y + rect.height || column >= rect.x + rect.width {
            return None;
        }
        let pad = self.padding.at(rect.width);
        let inner = rect.width.saturating_sub(2 * pad);
        // The box is a border row, the text rows, a border row.
        let text_rows = rect.height.saturating_sub(2).max(1);
        let visual = (row - rect.y).saturating_sub(1).min(text_rows - 1);
        let col = column.saturating_sub(rect.x + pad);
        let text = self.editor.get_text();
        let lines: Vec<&str> = text.split('\n').collect();
        let (cursor_row, _) = self.editor.cursor_position();
        Some(text_position(&lines, cursor_row, inner, visual, col))
    }

    /// Shows a worker's session in place of the manager's transcript and editor (#2252), or
    /// (`None`) the manager again. The transcript jumps to its latest output either way.
    pub fn show_session(&self, session: Option<SessionParts>) {
        self.transcript_slot.clear();
        self.session_slot.clear();
        self.editor_slot.clear();
        match session {
            Some(parts) => {
                self.transcript_slot.add_child(parts.transcript);
                self.session_slot.add_child(parts.bar);
                self.editor_slot.add_child(parts.keys);
            }
            None => {
                self.transcript_slot.add_child(self.transcript.clone());
                self.editor_slot.add_child(self.editor_view.clone());
            }
        }
        self.session_shown.store(
            !self.session_slot.get_children().is_empty(),
            std::sync::atomic::Ordering::SeqCst,
        );
        self.root.invalidate();
        self.scroll.to_end();
    }

    /// Whether a worker's session is shown.
    pub fn session_shown(&self) -> bool {
        self.session_shown.load(std::sync::atomic::Ordering::SeqCst)
    }

    /// This view's transcript, for showing it inside another view ([`SessionParts`]).
    pub fn transcript_component(&self) -> Arc<dyn Component> {
        self.transcript.clone()
    }

    /// Columns of space left and right of every area (`[tui] padding`, #2251; at most
    /// [`crate::MAX_PADDING`]). Narrow terminals get less ([`Padding::at`]).
    pub fn set_padding(&self, columns: usize) {
        self.padding.set(columns);
        self.root.invalidate();
    }

    /// The configured left and right padding.
    pub fn padding(&self) -> usize {
        self.padding.columns()
    }

    /// How assistant messages show their thinking (#2177): new ones, and (`/config thinking`,
    /// #2345) those already in the transcript, which open for `full` and close otherwise.
    pub fn set_show_thinking(&mut self, display: ThinkingDisplay) {
        self.show_thinking = display;
        for child in self.transcript.get_children() {
            if let Some(a) = child.as_any().downcast_ref::<AssistantBlock>() {
                a.set_display(display);
            }
        }
        self.transcript.invalidate();
    }

    pub fn show_thinking(&self) -> ThinkingDisplay {
        self.show_thinking
    }

    /// Whether clicks reach the chat (`[tui] mouse`); the last thinking's hint says so (#2345).
    pub fn set_mouse(&mut self, on: bool) {
        self.mouse = on;
        if let Some(last) = &self.last_thinking {
            last.set_hint(Some(self.thinking_hint()));
        }
    }

    fn thinking_hint(&self) -> &'static str {
        if self.mouse {
            THINKING_HINT
        } else {
            THINKING_HINT_KEY
        }
    }

    /// Expands or collapses the last thinking section (Ctrl+T). `None` when there is none yet,
    /// else whether it is now expanded.
    pub fn toggle_thinking(&self) -> Option<bool> {
        self.last_thinking.as_ref()?.toggle_thinking()
    }

    /// Ctrl+T on a `width` x `height` screen (#2345): expands or collapses the lowest thinking
    /// header in sight in the shown transcript (the manager's or a worker's), so scrolling to an
    /// older one and pressing Ctrl+T opens that one. `None` when no header is in sight.
    pub fn toggle_visible_thinking(&self, width: usize, height: usize) -> Option<bool> {
        let (rect, tw) = self.transcript_rect(width, height)?;
        let top = self.scroll.top();
        let (at, message, section) = self
            .thinking_headers(tw)
            .into_iter()
            .rfind(|(at, ..)| (top..top + rect.height).contains(at))?;
        self.toggle_header(at, &message, section, (width, height))
    }

    /// A left click at `(row, column)` (0-based) on a `width` x `height` screen (#2345): on a
    /// thinking header, opens or closes it. `None` when it isn't on one, else whether it is now
    /// expanded.
    pub fn click_thinking(&self, width: usize, height: usize, row: usize) -> Option<bool> {
        let (rect, tw) = self.transcript_rect(width, height)?;
        if row < rect.y || row >= rect.y + rect.height {
            return None;
        }
        let line = self.scroll.top() + (row - rect.y);
        let (at, message, section) = self
            .thinking_headers(tw)
            .into_iter()
            .find(|(at, ..)| *at == line)?;
        self.toggle_header(at, &message, section, (width, height))
    }

    /// The transcript's place on a `width` x `height` screen, and the width its entries render
    /// at (inside the scroll view's scrollbar column and the padding).
    fn transcript_rect(&self, width: usize, height: usize) -> Option<(rpi_tui::LayoutRect, usize)> {
        let frame = self.layout(width, height);
        let view: Arc<dyn Component> = self.scroll.view().clone();
        let rect = find_box(&frame.root, &view)?;
        let content = self.scroll.view().get_content_width(rect.width);
        Some((rect, content.saturating_sub(2 * self.padding.at(content))))
    }

    /// Every thinking header in the shown transcript at `width`: its line, its message and the
    /// section's index in it.
    fn thinking_headers(&self, width: usize) -> Vec<(usize, Arc<dyn Component>, usize)> {
        let mut out = Vec::new();
        let slot: Arc<dyn Component> = self.transcript_slot.clone();
        collect_headers(&slot, width, 0, &mut out);
        out
    }

    /// Toggles section `section` of `message`, whose header is on transcript line `at`, on a
    /// `screen` (width, height). While the view follows the end, an opened section that would
    /// push its header off the top is scrolled to instead, so it reads from its first line.
    fn toggle_header(
        &self,
        at: usize,
        message: &Arc<dyn Component>,
        section: usize,
        screen: (usize, usize),
    ) -> Option<bool> {
        let following = self.scroll.at_end();
        let expanded = message
            .as_any()
            .downcast_ref::<AssistantBlock>()?
            .toggle_section(section)?;
        if expanded && following {
            // A layout first, so the scroll view knows the new height (it stays at the end).
            self.layout(screen.0, screen.1);
            if at < self.scroll.top() {
                self.scroll.scroll_to(at);
            }
        }
        Some(expanded)
    }

    /// Expands or collapses every tool call's details (Ctrl+O, #2183); returns whether they are
    /// now expanded.
    pub fn toggle_tools(&self) -> bool {
        let expanded = self.tool_display.toggle();
        self.transcript.invalidate();
        expanded
    }

    /// Whether tool calls show their details.
    pub fn tools_expanded(&self) -> bool {
        self.tool_display.is_expanded()
    }

    /// Something visible went into the transcript: the next tool call starts a new group, below
    /// it.
    fn end_group(&self) {
        *self.group.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }

    /// The group a new call joins: the open one if every assistant message since it shows
    /// nothing (a message that only called tools, or hidden thinking), else a new one.
    fn tool_group(&self) -> Arc<ToolGroup> {
        let mut open = self.group.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(g) = open.as_ref()
            && g.after.iter().all(|a| a.render(200).is_empty())
        {
            return Arc::clone(&g.group);
        }
        let group = Arc::new(ToolGroup::new(Arc::clone(&self.tool_display)));
        self.transcript.add_child(group.clone());
        *open = Some(OpenGroup {
            group: Arc::clone(&group),
            after: Vec::new(),
        });
        group
    }

    /// Adds a tool call to the transcript, in the open group.
    fn add_tool(&self, name: &str, args: &serde_json::Value) -> Arc<ToolBlock> {
        let call = new_tool(name, args, Arc::clone(&self.tool_display));
        // #2347: a dispatch says what the worker is for, and how it is doing, under its line.
        if WORKER_TOOLS.contains(&name)
            && let Some(id) = amp_render::ticket_ids(args).first().copied()
        {
            let roster = self.roster.clone();
            call.set_live_row(Arc::new(move |width| roster.detail(id, width)));
        }
        self.tool_group().push(Arc::clone(&call));
        call
    }

    /// The layout root to hand to the TUI.
    pub fn root(&self) -> Arc<dyn Component> {
        Arc::clone(&self.root)
    }

    pub fn editor(&self) -> &Arc<Editor> {
        &self.editor
    }

    pub fn footer(&self) -> &Arc<Footer> {
        &self.footer
    }

    /// The AMP header and status line's inputs: give it a metadata source to show them.
    pub fn chrome(&self) -> &Chrome {
        &self.chrome
    }

    /// The AMP command panel (`/questions`, `/workers`, ...): open, close, read its selection.
    pub fn panel(&self) -> &Panel {
        &self.panel
    }

    /// The `/` completion menu above the editor.
    pub fn menu(&self) -> &CommandMenu {
        &self.menu
    }

    /// The transcript's scroll position: PageUp/PageDown, Ctrl+U/Ctrl+D, End, the wheel, and
    /// the "↓ new output" pill (#2185).
    pub fn scroll(&self) -> &TranscriptScroll {
        &self.scroll
    }

    /// Adds a component to the transcript (the ticket marker).
    pub fn add_to_transcript(&self, component: Arc<dyn Component>) {
        self.scroll.touch();
        self.end_group();
        self.transcript.add_child(component);
    }

    /// The header slot: the AMP header (2 lines, the second only while a ticket is current).
    pub fn header_slot(&self) -> &Arc<Container> {
        &self.header
    }

    /// The one-line AMP status line slot, between the editor and pi's footer.
    pub fn amp_status_slot(&self) -> &Arc<Container> {
        &self.amp_status
    }

    pub fn set_footer(&self, info: &FooterInfo) {
        self.footer.set_model(&info.model);
        self.footer.set_thinking_level(Some(&info.thinking));
        self.footer.set_context_window(info.context_window as i64);
        self.footer.set_cwd(&info.cwd);
        self.footer.set_git_branch(info.git_branch.as_deref());
        self.footer.set_session_name(info.session_name.as_deref());
    }

    /// Lines at the top of the transcript (pi's welcome banner).
    pub fn add_banner(&self, lines: &[String]) {
        if lines.is_empty() {
            return;
        }
        self.end_group();
        let colors = theme().colors;
        let mut text = Vec::with_capacity(lines.len());
        for (i, line) in lines.iter().enumerate() {
            text.push(if i == 0 {
                crate::density::strong(line)
            } else {
                colors.dim.fg(line)
            });
        }
        self.scroll.touch();
        // Compact (#2346): no blank rows around it; the first message keeps its one blank row.
        let compact = crate::density::compact();
        self.transcript.add_child(Arc::new(
            WrapText::new(text.join("\n"), 1, 1)
                .with_top_padding(!compact)
                .with_bottom_padding(!compact),
        ));
    }

    /// The person's own message. Sending one jumps back to the latest output, so it is in view.
    ///
    /// Every bubble sits one blank row away from what comes before it (the banner and notices
    /// end with one already) and from the reply after it ([`Self::start_assistant`] gives the
    /// reply a gap), so the first message and later ones look the same (#2188).
    pub fn add_user(&self, text: &str) {
        self.end_group();
        let block = UserBlock::new(text).with_margin_above(!self.transcript_ends_blank());
        self.transcript.add_child(Arc::new(block));
        self.scroll.to_end();
    }

    /// Whether the transcript's last row is blank (or there is nothing yet).
    fn transcript_ends_blank(&self) -> bool {
        for child in self.transcript.get_children().iter().rev() {
            if let Some(last) = child.render(80).last() {
                return rpi_tui::strip_ansi(last).trim().is_empty();
            }
        }
        true
    }

    /// A notice, warning or error: one blank row above and below it, so a run of them has one
    /// blank row between each, not two (#2334). Compact (#2346): the blank row above only.
    fn add_line(&self, text: &str, color: rpi_tui::theme::Color) {
        self.end_group();
        self.scroll.touch();
        let top = !self.transcript_ends_blank();
        self.transcript.add_child(Arc::new(
            WrapText::new(theme_glyphs(text, color), 1, 1)
                .with_top_padding(top)
                .with_bottom_padding(!crate::density::compact()),
        ));
    }

    /// A dim one-off line in the transcript (command output, run outcome).
    pub fn add_notice(&self, text: &str) {
        self.add_line(text, theme().colors.dim);
    }

    /// A warning line in the transcript (something the person should decide on).
    pub fn add_warning(&self, text: &str) {
        self.add_line(text, theme().colors.warning);
    }

    /// An error line in the transcript.
    pub fn add_error(&self, text: &str) {
        self.add_line(text, theme().colors.error);
    }

    /// Shows or hides pi's working spinner in the editor's top border.
    pub fn set_working(&self, working: bool) {
        if working {
            if self.editor.working().is_none() {
                self.editor.set_working(Some(WorkingState {
                    frame: 0,
                    started_at: Instant::now(),
                    message: "Working…".to_owned(),
                }));
            }
        } else if self.editor.working().is_some() {
            self.editor.set_working(None);
        }
    }

    /// The text next to the working spinner ("Working…", or a short hint), keeping the spinner's
    /// frame and elapsed time. Does nothing when no run is going.
    pub fn set_working_message(&self, message: &str) {
        if let Some(mut working) = self.editor.working()
            && working.message != message
        {
            working.message = message.to_owned();
            self.editor.set_working(Some(working));
        }
    }

    /// Shows `hint` dim in the empty editor in place of [`PLACEHOLDER`] (`None` puts the
    /// placeholder back). The shell's "Press Ctrl+C again to quit" (#2184).
    pub fn set_editor_hint(&self, hint: Option<&str>) {
        *self.hint.lock().unwrap_or_else(|e| e.into_inner()) = hint.map(str::to_owned);
    }

    /// The empty editor's usual hint ([`PLACEHOLDER`] unless set, #2349). It falls back to
    /// [`PLACEHOLDER`] where it doesn't fit.
    pub fn set_placeholder(&self, text: &'static str) {
        *self.placeholder.lock().unwrap_or_else(|e| e.into_inner()) = text;
    }

    /// The empty editor's usual hint.
    pub fn placeholder(&self) -> &'static str {
        *self.placeholder.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The hint the empty editor shows instead of [`PLACEHOLDER`], if any.
    pub fn editor_hint(&self) -> Option<String> {
        self.hint.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// The text next to the working spinner, if a run is going.
    pub fn working_message(&self) -> Option<String> {
        self.editor.working().map(|w| w.message)
    }

    /// The person asked the running reply to stop (see [`Self::stopping`]); cleared when the
    /// run has finished.
    pub fn set_stopping(&mut self, stopping: bool) {
        self.stopping = stopping;
    }

    /// Whether the person asked the running reply to stop.
    pub fn stopping(&self) -> bool {
        self.stopping
    }

    /// Whether a reply's error was shown since the last call (and forgets it).
    pub fn take_error_shown(&mut self) -> bool {
        std::mem::take(&mut self.error_shown)
    }

    /// Ticks the retry countdown; returns whether one is shown.
    pub fn tick(&self) -> bool {
        match &self.retry {
            Some(indicator) => {
                indicator.tick_countdown();
                true
            }
            None => false,
        }
    }

    /// Applies one rpi agent event to the transcript and footer.
    /// [`Self::apply`] for an event that happened `at` rather than now (#2355): a worker's
    /// session caught up from its feed or its file, so earlier tool calls and thinking show how
    /// long they took. `None`: not known, so they show no duration rather than `0 ms`.
    pub fn apply_at(&mut self, event: &AgentEvent, at: Option<Instant>) {
        self.event_time = Some(at);
        self.apply(event);
        self.event_time = None;
    }

    /// When the event being applied happened.
    fn event_time(&self) -> Option<Instant> {
        self.event_time.unwrap_or_else(|| Some(Instant::now()))
    }

    pub fn apply(&mut self, event: &AgentEvent) {
        self.scroll.touch();
        let at = self.event_time();
        self.chrome.observe(event);
        match event {
            AgentEvent::MessageStart {
                message: AgentMessage::Assistant(m),
            } => {
                self.clear_retry();
                let component = self.start_assistant();
                self.update_assistant(&component, &blocks(m));
            }
            AgentEvent::MessageUpdate { message, .. } => {
                let component = self.current_assistant();
                self.update_assistant(&component, &blocks(message));
            }
            AgentEvent::MessageEnd {
                message: AgentMessage::Assistant(m),
            } => {
                let component = self.current_assistant();
                component.finish_at(at);
                self.update_assistant(&component, &blocks(m));
                // A reply the person stopped keeps what it streamed, with no error line: the
                // shell adds one dim "Stopped." when the run ends (#2181).
                let stopped = matches!(m.stop_reason, StopReason::Aborted)
                    || (self.stopping && matches!(m.stop_reason, StopReason::Error));
                if !stopped && matches!(m.stop_reason, StopReason::Error | StopReason::Aborted) {
                    let error = match m.error_message.as_deref() {
                        // One sentence, never the provider's raw body (#2207).
                        Some(raw) => provider_error::summarize(&m.provider, raw),
                        None if matches!(m.stop_reason, StopReason::Aborted) => "aborted".into(),
                        None => "the model returned an error".into(),
                    };
                    component.update_error(&error, stop_reason_str(m.stop_reason));
                    self.error_shown = true;
                }
                self.assistant = None;
                self.add_usage(&m.usage);
            }
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } => {
                let component = self.add_tool(tool_name, args);
                component.set_running_at(at);
                self.tools.insert(tool_call_id.clone(), component);
            }
            AgentEvent::ToolExecutionEnd {
                tool_call_id,
                tool_name,
                result,
                is_error,
            } => {
                let component = match self.tools.remove(tool_call_id) {
                    Some(c) => c,
                    None => self.add_tool(tool_name, &serde_json::Value::Null),
                };
                let text = tool_result_text(result);
                if amp_render::is_amp_tool(tool_name)
                    && *is_error
                    && amp_core::approval::refusal(&text).is_some()
                {
                    // #2226: the shell's ApprovalNotice right under it says it all, with the
                    // server's sentence and the link: not the same sentence twice.
                    let line = amp_render::REFUSED_LINE;
                    component.set_result_at(line, line, true, at);
                } else if amp_render::is_amp_tool(tool_name) {
                    let summary = amp_render::result_summary(tool_name, &text, *is_error);
                    let detail = if *is_error {
                        amp_render::error_text(&text).to_owned()
                    } else {
                        pretty_json(&text)
                    };
                    component.set_result_at(&summary, &detail, *is_error, at);
                } else {
                    component.set_result_at(&output_summary(&text), &text, *is_error, at);
                }
            }
            AgentEvent::RetryScheduled {
                attempt,
                max_retries,
                delay_ms,
                ..
            } => {
                let indicator = Arc::new(StatusIndicator::retry(
                    *attempt,
                    *max_retries,
                    Duration::from_millis(*delay_ms),
                ));
                self.status.clear();
                self.status.add_child(indicator.clone());
                self.retry = Some(indicator);
            }
            AgentEvent::AgentEnd { .. } => {
                self.clear_retry();
                self.assistant = None;
            }
            _ => {}
        }
    }

    fn add_usage(&self, usage: &Usage) {
        self.footer.add_usage(
            usage.input,
            usage.output,
            usage.cache_read,
            usage.cache_write,
            usage.cost.total,
        );
        let context = usage.input + usage.output + usage.cache_read + usage.cache_write;
        if context > 0 {
            self.footer.set_context_tokens(Some(context));
        }
        let prompt = usage.input + usage.cache_read + usage.cache_write;
        if usage.cache_read > 0 && prompt > 0 {
            self.footer
                .set_cache_hit_rate(Some(usage.cache_read as f64 / prompt as f64 * 100.0));
        }
    }

    fn clear_retry(&mut self) {
        if self.retry.take().is_some() {
            self.status.clear();
        }
    }

    fn update_assistant(&mut self, component: &Arc<AssistantBlock>, blocks: &[Block]) {
        component.update_blocks_at(blocks, self.event_time());
        if component.has_thinking() {
            // Only the transcript's last thinking says how to open it (#2345).
            if let Some(old) = &self.last_thinking
                && !Arc::ptr_eq(old, component)
            {
                old.set_hint(None);
            }
            component.set_hint(Some(self.thinking_hint()));
            self.last_thinking = Some(Arc::clone(component));
        }
    }

    fn start_assistant(&mut self) -> Arc<AssistantBlock> {
        let component = Arc::new(AssistantBlock::with_thinking(self.show_thinking));
        // A reply right under the person's bubble keeps a blank row from it (#2188).
        if self
            .transcript
            .get_children()
            .last()
            .is_some_and(|c| c.as_any().is::<UserBlock>())
        {
            component.set_gap_above();
        }
        self.transcript.add_child(component.clone());
        // It goes below the open tool group; if it shows anything, the next call starts a new
        // group under it. Compact (#2346): right under the group's last call.
        if let Some(g) = self
            .group
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .as_mut()
        {
            if !crate::density::compact() {
                component.set_gap_above();
            }
            g.after.push(Arc::clone(&component));
        }
        self.assistant = Some(component.clone());
        component
    }

    fn current_assistant(&mut self) -> Arc<AssistantBlock> {
        match &self.assistant {
            Some(c) => Arc::clone(c),
            None => self.start_assistant(),
        }
    }

    /// Lays the whole screen out at `width` x `height` and returns its lines (with ANSI styling),
    /// with the "↓ new output" pill on the transcript's last row when it shows.
    pub fn render_frame(&self, width: usize, height: usize) -> Vec<String> {
        let frame = self.layout(width, height);
        let mut lines = frame.lines.clone();
        if let Some(pill) = self.scroll.pill() {
            paint_pill(&frame, &mut lines, pill);
        }
        lines
    }

    /// [`Self::render_frame`] without styling, right-trimmed: what the render tests compare.
    pub fn render_plain(&self, width: usize, height: usize) -> Vec<String> {
        self.render_frame(width, height)
            .iter()
            .map(|l| strip_ansi(l).trim_end().to_owned())
            .collect()
    }
}

/// pi's editor with [`PLACEHOLDER`] (or the shell's hint, [`ChatView::set_editor_hint`]) drawn
/// dim after the cursor while it is empty.
///
/// rpi-tui 0.8.0's own `EditorOptions::placeholder` can't be used: its cursor marker is inserted
/// inside the placeholder's `ESC[2m` (`insert_cursor_marker` ends an escape at `[`), so a real
/// terminal would print `2m/ for commands`, undimmed. Here the editor renders as usual and the
/// placeholder replaces the blank cells right after the cursor marker.
///
/// Without input focus ([`ChatView::set_input_focus`], #2344) the cursor marker is taken out,
/// so the terminal hides its cursor.
struct EditorView {
    editor: Arc<Editor>,
    hint: Arc<Mutex<Option<String>>>,
    placeholder: Arc<Mutex<&'static str>>,
    focus: Arc<AtomicBool>,
}

impl Component for EditorView {
    fn render(&self, width: usize) -> Vec<String> {
        // Compact (#2346): the editor's rules in the muted border colour, as the panels' are.
        let muted = crate::density::compact().then(|| theme().colors.border_muted);
        if self.editor.border_color() != muted {
            self.editor.set_border_color(muted);
        }
        let mut lines = self.placeholder(self.editor.render(width));
        if !self.focus.load(Ordering::Relaxed) {
            for line in &mut lines {
                *line = line.replace(CURSOR_MARKER, "");
            }
        }
        lines
    }

    fn invalidate(&self) {
        self.editor.invalidate();
    }

    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

impl EditorView {
    /// The editor's rows with the placeholder (or hint) after the cursor while it is empty.
    fn placeholder(&self, mut lines: Vec<String>) -> Vec<String> {
        if !self.editor.get_text().is_empty() {
            return lines;
        }
        let hint = self.hint.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let placeholder = *self.placeholder.lock().unwrap_or_else(|e| e.into_inner());
        // The shell's hint, else the usual one, else the short one where that doesn't fit.
        let texts: Vec<&str> = match hint.as_deref() {
            Some(hint) => vec![hint],
            None => vec![placeholder, PLACEHOLDER],
        };
        for line in &mut lines {
            let Some(at) = line.find(CURSOR_MARKER) else {
                continue;
            };
            let after = at + CURSOR_MARKER.len();
            let blanks = line[after..].bytes().take_while(|b| *b == b' ').count();
            if let Some(text) = texts.iter().find(|t| rpi_tui::visible_width(t) <= blanks) {
                let want = rpi_tui::visible_width(text);
                let dim = theme().colors.dim.fg(text);
                line.replace_range(after..after + want, &dim);
            }
            break;
        }
        lines
    }
}

/// Adds the thinking headers of `component` (rendered at `width`, its first row at transcript
/// line `base`) to `out`, looking inside containers; returns its rows.
fn collect_headers(
    component: &Arc<dyn Component>,
    width: usize,
    base: usize,
    out: &mut Vec<(usize, Arc<dyn Component>, usize)>,
) -> usize {
    if let Some(message) = component.as_any().downcast_ref::<AssistantBlock>() {
        for (row, section) in message.thinking_headers(width) {
            out.push((base + row, Arc::clone(component), section));
        }
        return component.render(width).len();
    }
    if let Some(container) = component.as_any().downcast_ref::<Container>() {
        let mut rows = 0;
        for child in container.get_children() {
            rows += collect_headers(&child, width, base + rows, out);
        }
        return rows;
    }
    component.render(width).len()
}

/// The rectangle of `target` in a laid-out frame.
fn find_box(node: &rpi_tui::LayoutBox, target: &Arc<dyn Component>) -> Option<rpi_tui::LayoutRect> {
    if std::ptr::addr_eq(Arc::as_ptr(&node.component), Arc::as_ptr(target)) {
        return Some(node.rect);
    }
    node.children.iter().find_map(|c| find_box(c, target))
}

/// The editor's wrapping (rpi-tui 0.8.0 `wrap_line_ranges`): byte ranges of `line` at most
/// `max` columns wide.
fn wrap_ranges(line: &str, max: usize) -> Vec<(usize, usize)> {
    let mut ranges = Vec::new();
    let (mut start, mut width) = (0, 0);
    for (i, ch) in line.char_indices() {
        let w = unicode_width::UnicodeWidthChar::width(ch).unwrap_or(0);
        if i > start && width + w > max {
            ranges.push((start, i));
            start = i;
            width = 0;
        }
        width += w;
    }
    ranges.push((start, line.len()));
    ranges
}

/// The `(line, byte offset)` at `visual` row (counted from the editor's first text row) and
/// `column` (from its left edge) of `lines` drawn `width` columns wide, as the editor draws
/// them: the cursor's line wraps a column early, the rest at the full width. A column inside a
/// wide character lands before it; past a row's end, at its end (on a wrapped row, before its
/// last character, so the cursor stays on that row).
fn text_position(
    lines: &[&str],
    cursor_row: usize,
    width: usize,
    visual: usize,
    column: usize,
) -> (usize, usize) {
    let width = width.max(1);
    let mut rows = Vec::new();
    for (i, line) in lines.iter().enumerate() {
        let max = if i == cursor_row {
            width.saturating_sub(1).max(1)
        } else {
            width
        };
        let ranges = wrap_ranges(line, max);
        let last = ranges.len() - 1;
        for (k, r) in ranges.into_iter().enumerate() {
            rows.push((i, r, k == last));
        }
    }
    let Some(&(line, (start, end), is_last)) = rows.get(visual).or(rows.last()) else {
        return (0, 0);
    };
    let text = lines[line];
    let mut at = start;
    let mut used = 0;
    for (i, ch) in text[start..end].char_indices() {
        let w = unicode_width::UnicodeWidthChar::width(ch).unwrap_or(0);
        if used + w > column {
            return (line, start + i);
        }
        used += w;
        at = start + i;
    }
    if is_last { (line, end) } else { (line, at) }
}

/// The open tool group and the assistant messages added after it.
struct OpenGroup {
    group: Arc<ToolGroup>,
    after: Vec<Arc<AssistantBlock>>,
}

/// A tool line: `complete_task #2078` plus a short argument summary for `amp_*` tools; the
/// tool's name and its main argument (path, pattern, command, ...) for others.
fn new_tool(name: &str, args: &serde_json::Value, display: Arc<ToolDisplay>) -> Arc<ToolBlock> {
    let call = if amp_render::is_amp_tool(name) {
        ToolBlock::with_display(
            amp_render::tool_title(name, args),
            amp_render::tool_summary(args),
            display,
        )
    } else {
        ToolBlock::with_display(name, tool_summary(args), display)
    };
    if args.as_object().is_some_and(|o| !o.is_empty()) {
        call.set_args_detail(clean(&args.to_string()));
    }
    Arc::new(call)
}

/// An AMP result for the expanded view: its JSON pretty-printed, or the text as is.
fn pretty_json(text: &str) -> String {
    serde_json::from_str::<serde_json::Value>(text.trim())
        .ok()
        .and_then(|v| serde_json::to_string_pretty(&v).ok())
        .unwrap_or_else(|| text.to_owned())
}

/// A non-AMP tool's result in one line: a short single line as is, else its line count
/// (`120 lines`); empty output says `no output`.
fn output_summary(text: &str) -> String {
    let lines: Vec<&str> = text.trim_end().lines().collect();
    match lines.as_slice() {
        [] => "no output".to_owned(),
        [one] if one.trim().is_empty() => "no output".to_owned(),
        [one] if rpi_tui::visible_width(one.trim()) <= 60 => clean(one),
        _ => format!(
            "{} line{}",
            lines.len(),
            if lines.len() == 1 { "" } else { "s" }
        ),
    }
}

/// A non-AMP tool's header summary: its main argument, on one line.
fn tool_summary(args: &serde_json::Value) -> String {
    const KEYS: &[&str] = &["path", "pattern", "command", "url", "query", "file_path"];
    KEYS.iter()
        .find_map(|k| args[*k].as_str().filter(|s| !s.trim().is_empty()))
        .map(|s| shorten(&clean(s.lines().next().unwrap_or("")), 120))
        .unwrap_or_default()
}

/// The visible blocks of an assistant message (tool calls render as their own components).
fn blocks(m: &AssistantMessage) -> Vec<Block> {
    m.content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(Block::Text(t.text.clone())),
            Content::Thinking(t) => Some(Block::Thinking(t.thinking.clone())),
            _ => None,
        })
        .collect()
}

/// `text` in `base`, with the emoji status glyphs a system line may carry (`✅`, `❌`, `⚠️`)
/// mapped to the theme's `✓`, `✗` and `⚠` in the success, error and warning colours: the emoji
/// render wider and heavier than the rest of the UI (#2188).
pub fn theme_glyphs(text: &str, base: rpi_tui::theme::Color) -> String {
    let colors = theme().colors;
    let mut out = String::new();
    let mut run = String::new();
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        let glyph = match c {
            '✅' | '✔' => Some(colors.success.fg("✓")),
            '❌' | '✖' => Some(colors.error.fg("✗")),
            '⚠' => Some(colors.warning.fg("⚠")),
            _ => None,
        };
        match glyph {
            Some(g) => {
                if chars.peek() == Some(&'\u{fe0f}') {
                    chars.next();
                }
                if !run.is_empty() {
                    out.push_str(&base.fg(&std::mem::take(&mut run)));
                }
                out.push_str(&g);
            }
            None => run.push(c),
        }
    }
    if !run.is_empty() || out.is_empty() {
        out.push_str(&base.fg(&run));
    }
    out
}
