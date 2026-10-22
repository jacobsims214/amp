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
//! 8. pi's two-line **footer**, unchanged (cwd/branch/session; usage and model • thinking).
//!
//! Nothing here touches a terminal: [`ChatView::render_frame`] lays the screen out at any size,
//! which is what the render tests (and `amp-harness --snapshot`) use. The header and status line
//! render nothing until [`ChatView::chrome`] has a metadata source.
//!
//! `amp_*` tool calls are titled `AMP · complete_task #2078` with a compact result
//! ([`crate::amp_render`]).
//!
//! The transcript's entries are [`crate::transcript`]'s components, which look like pi's but
//! word-wrap (#2176): rpi-tui 0.8.0's own components break lines mid-word.

use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::events::{stop_reason_str, tool_result_text};

use crate::amp_render;
use crate::chrome::{Chrome, clean, shorten};
use crate::overlay::Panel;
use crate::slash::CommandMenu;
use crate::transcript::{AssistantBlock, Block, ThinkingDisplay, ToolBlock, UserBlock, WrapText};
use rpi_agent::{AgentEvent, AgentMessage};
use rpi_ai::types::{AssistantMessage, Content, StopReason, Usage};
use rpi_tui::{
    CURSOR_MARKER, Component, Container, Editor, EditorOptions, EditorStyle, Focusable,
    FooterComponent, Keybindings, ScrollView, StackChild, StackEntry, StatusIndicator, VStack,
    WorkingState, strip_ansi, theme,
};

/// The dim hint in the empty editor (#2182).
pub const PLACEHOLDER: &str = "/ for commands";

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

/// The chat screen's components and transcript state.
pub struct ChatView {
    header: Arc<Container>,
    transcript: Arc<Container>,
    status: Arc<Container>,
    editor: Arc<Editor>,
    amp_status: Arc<Container>,
    footer: Arc<FooterComponent>,
    /// The AMP header and status line's inputs (M3-3).
    chrome: Chrome,
    /// The AMP command panel (M3-4), above the editor while open.
    panel: Panel,
    /// The `/` completion menu (#2182), right above the editor while a command is being typed.
    menu: CommandMenu,
    root: Arc<dyn Component>,
    /// The assistant message being streamed, if any.
    assistant: Option<Arc<AssistantBlock>>,
    /// Running tool calls by id.
    tools: HashMap<String, Arc<ToolBlock>>,
    /// The retry indicator in the status slot, if one is shown.
    retry: Option<Arc<StatusIndicator>>,
    /// How thinking shows (`[tui] show_thinking`, #2177).
    show_thinking: ThinkingDisplay,
    /// The last assistant message with thinking: what Ctrl+T expands.
    last_thinking: Option<Arc<AssistantBlock>>,
    /// The person asked the run to stop: its aborted (or failed) reply keeps what it streamed
    /// and shows no error, since the shell says "Stopped." once (#2181).
    stopping: bool,
    /// What the empty editor says instead of [`PLACEHOLDER`] for a moment ("Press Ctrl+C again
    /// to quit", #2184).
    hint: Arc<Mutex<Option<String>>>,
}

impl Default for ChatView {
    fn default() -> Self {
        Self::new()
    }
}

impl ChatView {
    pub fn new() -> Self {
        let chrome = Chrome::new();
        let header = Arc::new(Container::new());
        header.add_child(chrome.header());
        let transcript = Arc::new(Container::new());
        let scroll = Arc::new(ScrollView::primary(transcript.clone()));
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
        let editor_slot = Arc::new(Container::new());
        editor_slot.add_child(Arc::new(EditorView {
            editor: editor.clone(),
            hint: Arc::clone(&hint),
        }));
        let amp_status = Arc::new(Container::new());
        amp_status.add_child(chrome.status_line());
        let footer = Arc::new(FooterComponent::new());
        footer.set_hints("Enter send · Shift+Enter new line · Esc abort · Ctrl+C quit · /help");

        let root = VStack::from_children(vec![
            StackChild::Entry(StackEntry::new(header.clone()).shrink(0)),
            StackChild::Entry(
                StackEntry::new(scroll)
                    .basis(0)
                    .grow(1)
                    .shrink(1)
                    .min_size(1),
            ),
            StackChild::Entry(StackEntry::new(status.clone())),
            StackChild::Entry(StackEntry::new(panel_slot).shrink(1)),
            StackChild::Entry(StackEntry::new(menu_slot).shrink(1)),
            StackChild::Entry(StackEntry::new(editor_slot).shrink(0).min_size(3)),
            StackChild::Entry(StackEntry::new(amp_status.clone()).shrink(0)),
            StackChild::Entry(StackEntry::new(footer.clone())),
        ]);
        Self {
            header,
            transcript,
            status,
            editor,
            amp_status,
            footer,
            chrome,
            panel,
            menu,
            root: Arc::new(root),
            assistant: None,
            tools: HashMap::new(),
            retry: None,
            show_thinking: ThinkingDisplay::default(),
            last_thinking: None,
            stopping: false,
            hint,
        }
    }

    /// How new assistant messages show their thinking (#2177).
    pub fn set_show_thinking(&mut self, display: ThinkingDisplay) {
        self.show_thinking = display;
    }

    pub fn show_thinking(&self) -> ThinkingDisplay {
        self.show_thinking
    }

    /// Expands or collapses the last thinking section (Ctrl+T). `None` when there is none yet,
    /// else whether it is now expanded.
    pub fn toggle_thinking(&self) -> Option<bool> {
        self.last_thinking.as_ref()?.toggle_thinking()
    }

    /// The layout root to hand to the TUI.
    pub fn root(&self) -> Arc<dyn Component> {
        Arc::clone(&self.root)
    }

    pub fn editor(&self) -> &Arc<Editor> {
        &self.editor
    }

    pub fn footer(&self) -> &Arc<FooterComponent> {
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

    /// Adds a component to the transcript (the ticket marker).
    pub fn add_to_transcript(&self, component: Arc<dyn Component>) {
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
        let colors = theme().colors;
        let mut text = Vec::with_capacity(lines.len());
        for (i, line) in lines.iter().enumerate() {
            text.push(if i == 0 {
                rpi_tui::bold(line)
            } else {
                colors.dim.fg(line)
            });
        }
        self.transcript
            .add_child(Arc::new(WrapText::new(text.join("\n"), 1, 1)));
    }

    pub fn add_user(&self, text: &str) {
        self.transcript.add_child(Arc::new(UserBlock::new(text)));
    }

    /// A dim one-off line in the transcript (command output, run outcome).
    pub fn add_notice(&self, text: &str) {
        let colors = theme().colors;
        self.transcript
            .add_child(Arc::new(WrapText::new(colors.dim.fg(text), 1, 1)));
    }

    /// An error line in the transcript.
    pub fn add_error(&self, text: &str) {
        let colors = theme().colors;
        self.transcript
            .add_child(Arc::new(WrapText::new(colors.error.fg(text), 1, 1)));
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
        self.footer.set_status("");
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
    pub fn apply(&mut self, event: &AgentEvent) {
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
                component.finish();
                self.update_assistant(&component, &blocks(m));
                // A reply the person stopped keeps what it streamed, with no error line: the
                // shell adds one dim "Stopped." when the run ends (#2181).
                let stopped = matches!(m.stop_reason, StopReason::Aborted)
                    || (self.stopping && matches!(m.stop_reason, StopReason::Error));
                if !stopped && matches!(m.stop_reason, StopReason::Error | StopReason::Aborted) {
                    let error = m.error_message.as_deref().unwrap_or(match m.stop_reason {
                        StopReason::Aborted => "aborted",
                        _ => "the model returned an error",
                    });
                    component.update_error(error, stop_reason_str(m.stop_reason));
                }
                self.assistant = None;
                self.add_usage(&m.usage);
            }
            AgentEvent::ToolExecutionStart {
                tool_call_id,
                tool_name,
                args,
            } => {
                let component = new_tool(tool_name, args);
                component.set_running();
                self.transcript.add_child(component.clone());
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
                    None => {
                        let c = new_tool(tool_name, &serde_json::Value::Null);
                        self.transcript.add_child(c.clone());
                        c
                    }
                };
                let text = tool_result_text(result);
                if amp_render::is_amp_tool(tool_name) {
                    component.set_result(&amp_render::result_text(&text, *is_error), *is_error);
                } else {
                    component.set_result(&text, *is_error);
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
        component.update_blocks(blocks);
        if component.has_thinking() {
            self.last_thinking = Some(Arc::clone(component));
        }
    }

    fn start_assistant(&mut self) -> Arc<AssistantBlock> {
        let component = Arc::new(AssistantBlock::with_thinking(self.show_thinking));
        self.transcript.add_child(component.clone());
        self.assistant = Some(component.clone());
        component
    }

    fn current_assistant(&mut self) -> Arc<AssistantBlock> {
        match &self.assistant {
            Some(c) => Arc::clone(c),
            None => self.start_assistant(),
        }
    }

    /// Lays the whole screen out at `width` x `height` and returns its lines (with ANSI styling).
    pub fn render_frame(&self, width: usize, height: usize) -> Vec<String> {
        rpi_tui::render_layout_frame(self.root(), width, height).lines
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
struct EditorView {
    editor: Arc<Editor>,
    hint: Arc<Mutex<Option<String>>>,
}

impl Component for EditorView {
    fn render(&self, width: usize) -> Vec<String> {
        let mut lines = self.editor.render(width);
        if !self.editor.get_text().is_empty() {
            return lines;
        }
        let hint = self.hint.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let text = hint.as_deref().unwrap_or(PLACEHOLDER);
        let want = rpi_tui::visible_width(text);
        for line in &mut lines {
            let Some(at) = line.find(CURSOR_MARKER) else {
                continue;
            };
            let after = at + CURSOR_MARKER.len();
            let blanks = line[after..].bytes().take_while(|b| *b == b' ').count();
            if blanks >= want {
                let dim = theme().colors.dim.fg(text);
                line.replace_range(after..after + want, &dim);
            }
            break;
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

/// A tool block: `AMP · complete_task #2078` plus a short argument summary for `amp_*` tools;
/// the tool's name and its main argument (path, pattern, command, ...) for others.
fn new_tool(name: &str, args: &serde_json::Value) -> Arc<ToolBlock> {
    if amp_render::is_amp_tool(name) {
        Arc::new(ToolBlock::new(
            amp_render::tool_title(name, args),
            amp_render::tool_summary(args),
        ))
    } else {
        Arc::new(ToolBlock::new(name, tool_summary(args)))
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
