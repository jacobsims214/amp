//! The slash commands' one list (#2182): what the `/` completion menu offers and what `/help`
//! shows, from the same table, so the two never disagree.
//!
//! - [`COMMANDS`]: each command's name, arguments, one-line description and group (AMP first,
//!   then Chat), and its aliases (`/q`, `/exit`, `/?`).
//! - [`CommandMenu`]: the menu above the editor. It opens when the editor holds `/` and a command
//!   name being typed (one line, no space yet), filtered by prefix. ↑↓ move, Tab completes the
//!   selected command into the editor, Enter runs it, Esc dismisses the menu until the text
//!   changes. It reads the editor at render time, so it follows every keystroke without a hook.
//! - [`HelpBlock`]: `/help` as a grouped two-column list (AMP, Config, Chat, Keys) at full contrast, the
//!   descriptions word-wrapped under their column ([`crate::wrap`]); on a narrow screen each
//!   description goes under its command instead.

use std::{
    any::Any,
    sync::{Arc, Mutex},
};

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

use crate::newline::NewlineMode;
use rpi_tui::{Component, Editor, bold, theme, visible_width};

use crate::chrome::shorten;
use crate::wrap::wrap;

/// Where a command is listed in `/help` (and in which order in the menu).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Group {
    Amp,
    /// Models, providers, MCP servers, agents, skills and settings (#2245).
    Config,
    Chat,
}

impl Group {
    pub fn title(self) -> &'static str {
        match self {
            Group::Amp => "AMP",
            Group::Config => "Config",
            Group::Chat => "Chat",
        }
    }
}

/// One slash command.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CommandSpec {
    /// Without the slash: `ticket`.
    pub name: &'static str,
    /// The arguments as shown (`[id|none]`); empty when it takes none.
    pub args: &'static str,
    /// One line: what it does.
    pub about: &'static str,
    pub group: Group,
    /// Other names that run it (`q`, `exit`).
    pub aliases: &'static [&'static str],
}

impl CommandSpec {
    const fn new(
        group: Group,
        name: &'static str,
        args: &'static str,
        about: &'static str,
    ) -> Self {
        Self {
            name,
            args,
            about,
            group,
            aliases: &[],
        }
    }

    const fn aliases(mut self, aliases: &'static [&'static str]) -> Self {
        self.aliases = aliases;
        self
    }

    /// `/ticket [id|none]`.
    pub fn usage(&self) -> String {
        if self.args.is_empty() {
            format!("/{}", self.name)
        } else {
            format!("/{} {}", self.name, self.args)
        }
    }

    /// What Tab puts in the editor: `/ticket ` (ready for the argument) or `/usage`.
    pub fn completion(&self) -> String {
        if self.args.is_empty() {
            format!("/{}", self.name)
        } else {
            format!("/{} ", self.name)
        }
    }

    fn named(&self, name: &str) -> bool {
        self.name == name || self.aliases.contains(&name)
    }

    fn starts_with(&self, prefix: &str) -> bool {
        self.name.starts_with(prefix) || self.aliases.iter().any(|a| a.starts_with(prefix))
    }
}

/// Every slash command, AMP first. `Shell::command` runs them; keep the two in step (a test
/// checks that each one here is known to the shell).
pub const COMMANDS: &[CommandSpec] = &[
    CommandSpec::new(
        Group::Amp,
        "amp",
        "",
        "the AMP connection: server, user, MCP health",
    ),
    CommandSpec::new(
        Group::Amp,
        "ticket",
        "[id|none]",
        "the current ticket; switch it, or clear it",
    ),
    CommandSpec::new(
        Group::Amp,
        "workers",
        "",
        "workers here, and tickets in progress elsewhere",
    ),
    CommandSpec::new(
        Group::Amp,
        "worker",
        "[id]",
        "watch a worker: Esc back, Tab, x stop, o AMP",
    ),
    CommandSpec::new(
        Group::Amp,
        "questions",
        "",
        "tickets waiting on an answer; answer them here",
    ),
    CommandSpec::new(
        Group::Amp,
        "plan",
        "",
        "plan approval and the epics and stories",
    ),
    CommandSpec::new(
        Group::Amp,
        "project",
        "[id]",
        "the project and its board; switch to another",
    ),
    CommandSpec::new(
        Group::Amp,
        "open",
        "[id|plan]",
        "open the ticket (or the plan) in the browser",
    ),
    CommandSpec::new(
        Group::Amp,
        "server",
        "[name|address]",
        "the AMP servers; switch to another",
    ),
    CommandSpec::new(
        Group::Amp,
        "remote",
        "[on|off|pair]",
        "chat from a phone; /remote copy, open, devices",
    ),
    CommandSpec::new(
        Group::Config,
        "model",
        "[p/m|roles|role]",
        "this chat's model; /model roles: every role's",
    ),
    CommandSpec::new(
        Group::Config,
        "connect",
        "[provider]",
        "model providers and their keys; add a key",
    ),
    CommandSpec::new(
        Group::Config,
        "mcp",
        "[name|action name]",
        "MCP servers: health, restart, enable, trust",
    ),
    CommandSpec::new(
        Group::Config,
        "agents",
        "[id]",
        "agents: model, skills, which file they're from",
    ),
    CommandSpec::new(
        Group::Config,
        "skills",
        "[name]",
        "skills: what each does, where it's from",
    ),
    CommandSpec::new(
        Group::Config,
        "config",
        "[thinking]",
        "every setting, where it's from; thinking mode",
    ),
    CommandSpec::new(
        Group::Config,
        "reload",
        "",
        "reload agents, skills, models, MCP servers",
    ),
    CommandSpec::new(Group::Chat, "usage", "", "tokens and cost this session"),
    CommandSpec::new(Group::Chat, "abort", "", "stop the reply"),
    CommandSpec::new(
        Group::Chat,
        "stop",
        "",
        "stop the reply, or the wait for plan approval",
    ),
    CommandSpec::new(Group::Chat, "close", "", "close the open panel"),
    CommandSpec::new(Group::Chat, "help", "", "this list").aliases(&["?"]),
    CommandSpec::new(Group::Chat, "quit", "", "leave the chat").aliases(&["exit", "q"]),
];

/// `/help`'s Keys group with mouse capture on (the default): (keys, what they do). The last row
/// is the terminal's selection, which needs a modifier while the terminal reports the mouse.
/// [`keys`] gives the rows for either setting.
pub const KEYS: &[(&str, &str)] = &[
    ("Enter", "send"),
    ("Shift+Enter", "new line"),
    ("Ctrl+J", "new line, in every terminal"),
    ("Alt+Enter", "new line (on a Mac, with ⌥ as Esc+ or Meta)"),
    ("\\ Enter", "new line: Enter right after a \\"),
    ("↑/↓", "earlier prompts (empty or one-line editor)"),
    ("/", "commands: ↑↓ choose, Tab complete, Enter run"),
    ("Esc", "stop the reply, or close a panel or session"),
    ("Ctrl+C", "stop the reply, clear the editor; twice quits"),
    ("Ctrl+D", "quit (empty editor, at the latest output)"),
    ("Ctrl+T", "open or close the thinking in sight, or click it"),
    ("Ctrl+O", "show or hide tool call details"),
    ("PgUp/PgDn", "scroll the transcript a page"),
    ("Ctrl+U/D", "scroll half a page (with an empty editor)"),
    ("End", "jump to the latest output (with an empty editor)"),
    ("Ctrl+W", "pick a running worker: ←/→, Enter watch"),
    ("Ctrl+X/G", "in /workers: stop the worker / open it in AMP"),
    ("click", "a worker or its #ticket: watch its session"),
    ("Fn/⌥-drag", "select text (Fn in Terminal.app, ⌥ in iTerm2)"),
];

/// The Shift+Enter row where the terminal has no kitty keyboard protocol (#2349): it sends the
/// same CR as Enter there.
pub const LEGACY_SHIFT_ENTER: (&str, &str) = (
    "Shift+Enter",
    "sends, like Enter (no kitty keyboard protocol here): use Ctrl+J",
);

/// The Keys row for clicking a worker (#2347): only while the terminal reports the mouse.
pub const CLICK_KEY: &str = "click";

/// The Keys row for selecting text with mouse capture off (`[tui] mouse = false`, #2185): plain
/// click-drag selects, so no modifier to name.
pub const DRAG_KEY: (&str, &str) = ("drag", "select text");

/// `/help`'s Keys group for the mouse-capture setting: [`KEYS`] when on; off, its last row (the
/// modifier-drag) becomes [`DRAG_KEY`] and clicking a worker ([`CLICK_KEY`]) goes.
pub fn keys(mouse_capture: bool) -> Vec<(&'static str, &'static str)> {
    keys_for(mouse_capture, NewlineMode::Kitty)
}

/// [`keys`] for this terminal's new-line keys ([`NewlineMode`], #2349): where Shift+Enter sends,
/// its row says so ([`LEGACY_SHIFT_ENTER`]).
pub fn keys_for(mouse_capture: bool, newline: NewlineMode) -> Vec<(&'static str, &'static str)> {
    let mut rows = KEYS.to_vec();
    if !mouse_capture {
        rows.pop();
        rows.retain(|(k, _)| *k != CLICK_KEY);
        rows.push(DRAG_KEY);
    }
    if !newline.shift_enter() {
        for row in &mut rows {
            if row.0 == LEGACY_SHIFT_ENTER.0 {
                *row = LEGACY_SHIFT_ENTER;
            }
        }
    }
    rows
}

/// The command named `name` (or one of its aliases).
pub fn find(name: &str) -> Option<&'static CommandSpec> {
    COMMANDS.iter().find(|c| c.named(name))
}

/// The commands whose name (or an alias) starts with `prefix`, in [`COMMANDS`] order.
pub fn matching(prefix: &str) -> Vec<&'static CommandSpec> {
    COMMANDS.iter().filter(|c| c.starts_with(prefix)).collect()
}

/// The command name being typed, when the editor holds only `/` and a name so far (one line, no
/// space): `"/ti"` -> `Some("ti")`, `"/"` -> `Some("")`.
pub fn typed_name(text: &str) -> Option<&str> {
    let name = text.strip_prefix('/')?;
    (!name.contains(char::is_whitespace)).then_some(name)
}

/// At most this many commands are listed at once; the list scrolls to the selection.
const MENU_ROWS: usize = 8;

#[derive(Debug, Default)]
struct MenuState {
    /// The selected row among the matches.
    selected: usize,
    /// The editor text the selection belongs to; a new text starts at the top again.
    text: String,
    /// The editor text Esc dismissed the menu at; it stays hidden until the text changes.
    dismissed: Option<String>,
}

/// What a key did to the menu ([`CommandMenu::key`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MenuKey {
    /// The menu isn't showing, or the key isn't one of its keys: the editor gets it.
    Pass,
    /// The menu used it (moved, completed, dismissed).
    Used,
    /// Enter: the editor now holds the selected command; let the editor submit it.
    Run,
}

/// The `/` completion menu, docked above the editor. Cloning shares the state.
#[derive(Clone)]
pub struct CommandMenu {
    editor: Arc<Editor>,
    state: Arc<Mutex<MenuState>>,
}

impl CommandMenu {
    pub fn new(editor: Arc<Editor>) -> Self {
        Self {
            editor,
            state: Arc::new(Mutex::new(MenuState::default())),
        }
    }

    /// The commands the menu shows now (empty when it is closed), and the selected one's index.
    pub fn items(&self) -> (Vec<&'static CommandSpec>, usize) {
        let text = self.editor.get_text();
        let Ok(mut state) = self.state.lock() else {
            return (Vec::new(), 0);
        };
        if state.text != text {
            state.text = text.clone();
            state.selected = 0;
            if state.dismissed.as_deref() != Some(text.as_str()) {
                state.dismissed = None;
            }
        }
        if state.dismissed.is_some() {
            return (Vec::new(), 0);
        }
        let items = typed_name(&text).map(matching).unwrap_or_default();
        let selected = state.selected.min(items.len().saturating_sub(1));
        (items, selected)
    }

    /// Whether the menu is showing.
    pub fn is_open(&self) -> bool {
        !self.items().0.is_empty()
    }

    /// The selected command, while the menu shows.
    pub fn selected(&self) -> Option<&'static CommandSpec> {
        let (items, selected) = self.items();
        items.get(selected).copied()
    }

    /// A key while the menu may be showing: ↑↓ (and Ctrl+P/N) move, Tab completes, Enter runs
    /// the selected command, Esc dismisses. Other keys belong to the editor.
    pub fn key(&self, key: &KeyEvent) -> MenuKey {
        let (items, selected) = self.items();
        if items.is_empty() {
            return MenuKey::Pass;
        }
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        if key
            .modifiers
            .intersects(KeyModifiers::ALT | KeyModifiers::SHIFT)
            && !matches!(key.code, KeyCode::Char(_))
        {
            return MenuKey::Pass;
        }
        let n = items.len();
        let set = |i: usize| {
            if let Ok(mut s) = self.state.lock() {
                s.selected = i;
            }
        };
        match key.code {
            KeyCode::Up if !ctrl => set((selected + n - 1) % n),
            KeyCode::Down if !ctrl => set((selected + 1) % n),
            KeyCode::Char('p') if ctrl => set((selected + n - 1) % n),
            KeyCode::Char('n') if ctrl => set((selected + 1) % n),
            KeyCode::Tab if !ctrl => self.fill(&items[selected].completion()),
            KeyCode::Enter if !ctrl => {
                // A full name or alias typed out runs as typed (`/q` quits; it doesn't pick
                // `/questions`).
                let text = self.editor.get_text();
                if typed_name(&text).and_then(find).is_none() {
                    self.fill(&format!("/{}", items[selected].name));
                }
                return MenuKey::Run;
            }
            KeyCode::Esc if !ctrl => {
                if let Ok(mut s) = self.state.lock() {
                    s.dismissed = Some(self.editor.get_text());
                }
            }
            _ => return MenuKey::Pass,
        }
        MenuKey::Used
    }

    /// Puts `text` in the editor with the cursor at its end.
    fn fill(&self, text: &str) {
        self.editor.set_text(text);
        self.editor.set_cursor(0, text.len());
    }
}

impl Component for CommandMenu {
    fn render(&self, width: usize) -> Vec<String> {
        let (items, selected) = self.items();
        if items.is_empty() || width < 4 {
            return Vec::new();
        }
        let c = theme().colors;
        let rule_char = theme().border_style.chars().horizontal;
        let title = " commands ";
        let fill = width.saturating_sub(2 + visible_width(title));
        let mut out = vec![format!(
            "{}{}{}",
            c.border_muted.fg(&rule_char.repeat(2)),
            bold(&c.text.fg(title)),
            c.border_muted.fg(&rule_char.repeat(fill))
        )];
        let inner = width.saturating_sub(2);
        let col = items
            .iter()
            .map(|i| visible_width(&i.usage()))
            .max()
            .unwrap_or(0)
            + 3;
        let start = selected.saturating_sub(MENU_ROWS - 1);
        let end = (start + MENU_ROWS).min(items.len());
        if start > 0 {
            out.push(c.dim.fg(&format!("  ↑ {start} more")));
        }
        for (i, item) in items.iter().enumerate().take(end).skip(start) {
            let is_selected = i == selected;
            let marker = if is_selected {
                c.accent.fg("›")
            } else {
                " ".to_owned()
            };
            let name = shorten(&format!("/{}", item.name), inner);
            let args_room = inner.saturating_sub(visible_width(&name));
            let args = if item.args.is_empty() || args_room < 2 {
                String::new()
            } else {
                shorten(&format!(" {}", item.args), args_room)
            };
            let used = visible_width(&name) + visible_width(&args);
            let name = if is_selected {
                bold(&c.accent.fg(&name))
            } else {
                c.text.fg(&name)
            };
            let mut line = format!("{marker} {name}{}", c.muted.fg(&args));
            if inner > col + 8 {
                let about = shorten(item.about, inner - col);
                line.push_str(&" ".repeat(col.saturating_sub(used)));
                line.push_str(&c.dim.fg(&about));
            }
            out.push(line);
        }
        if end < items.len() {
            out.push(c.dim.fg(&format!("  ↓ {} more", items.len() - end)));
        }
        out.push(format!(
            "  {}",
            c.dim.fg(&shorten(
                "↑↓ choose · Tab complete · Enter run · Esc dismiss",
                inner
            ))
        ));
        out
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// `/help`: AMP, Config, Chat and Keys, each a two-column list (command and arguments, then what it
/// does), at full contrast. Descriptions word-wrap under their column; below
/// [`HelpBlock::TWO_COLUMNS`] cells of description room they go under the command instead.
/// The Keys group follows the mouse-capture setting ([`keys`]).
#[derive(Debug, Clone, Copy)]
pub struct HelpBlock {
    mouse_capture: bool,
    newline: NewlineMode,
}

impl Default for HelpBlock {
    fn default() -> Self {
        Self::new()
    }
}

impl HelpBlock {
    /// The least description room (in cells) for the two-column layout.
    pub const TWO_COLUMNS: usize = 24;

    /// `/help` with mouse capture on (the default).
    pub fn new() -> Self {
        Self {
            mouse_capture: true,
            newline: NewlineMode::Kitty,
        }
    }

    /// `/help` for this terminal's new-line keys (#2349).
    pub fn newline(mut self, mode: NewlineMode) -> Self {
        self.newline = mode;
        self
    }

    /// `/help` for this mouse-capture setting (`[tui] mouse`, #2185).
    pub fn mouse_capture(mut self, on: bool) -> Self {
        self.mouse_capture = on;
        self
    }

    /// The rows of each group: (group title, [(left column, description)]).
    fn groups(&self) -> Vec<(&'static str, Vec<(String, &'static str)>)> {
        let mut groups = Vec::new();
        for group in [Group::Amp, Group::Config, Group::Chat] {
            let rows = COMMANDS
                .iter()
                .filter(|c| c.group == group)
                .map(|c| (c.usage(), c.about))
                .collect();
            groups.push((group.title(), rows));
        }
        groups.push((
            "Keys",
            keys_for(self.mouse_capture, self.newline)
                .into_iter()
                .map(|(k, about)| (k.to_owned(), about))
                .collect(),
        ));
        groups
    }
}

/// Spaces on the left of the group titles, and of the rows under them.
const HELP_PAD: usize = 1;
const HELP_INDENT: usize = 3;

impl Component for HelpBlock {
    fn render(&self, width: usize) -> Vec<String> {
        let c = theme().colors;
        let groups = self.groups();
        let col = groups
            .iter()
            .flat_map(|(_, rows)| rows.iter().map(|(left, _)| visible_width(left)))
            .max()
            .unwrap_or(0)
            + 2;
        let inner = width.saturating_sub(HELP_INDENT).max(1);
        let two_columns = inner >= col + Self::TWO_COLUMNS;
        // Compact (#2346): no blank rows between the groups or after the list.
        let compact = crate::density::compact();
        let mut out = vec![String::new()];
        for (g, (title, rows)) in groups.iter().enumerate() {
            if g > 0 && !compact {
                out.push(String::new());
            }
            out.push(format!(
                "{}{}",
                " ".repeat(HELP_PAD),
                bold(&c.accent.fg(title))
            ));
            for (left, about) in rows {
                let (head, args) = match left.split_once(' ') {
                    Some((head, args)) if left.starts_with('/') => (head, args),
                    _ => (left.as_str(), ""),
                };
                let styled = if args.is_empty() {
                    c.text.fg(head)
                } else {
                    format!("{} {}", c.text.fg(head), c.muted.fg(args))
                };
                let indent = " ".repeat(HELP_INDENT);
                if two_columns {
                    let pad = " ".repeat(col - visible_width(left));
                    for (i, part) in wrap(about, inner - col).into_iter().enumerate() {
                        if i == 0 {
                            out.push(format!("{indent}{styled}{pad}{}", c.text.fg(&part)));
                        } else {
                            out.push(format!("{indent}{}{}", " ".repeat(col), c.text.fg(&part)));
                        }
                    }
                } else {
                    out.push(format!("{indent}{styled}"));
                    let under = " ".repeat(HELP_INDENT + 2);
                    for part in wrap(about, width.saturating_sub(HELP_INDENT + 2).max(1)) {
                        out.push(format!("{under}{}", c.text.fg(&part)));
                    }
                }
            }
        }
        if !compact {
            out.push(String::new());
        }
        out
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn typing_filters_by_prefix() {
        assert_eq!(typed_name("/"), Some(""));
        assert_eq!(typed_name("/ti"), Some("ti"));
        assert_eq!(typed_name("/ticket 2"), None);
        assert_eq!(typed_name("hello"), None);
        let names = |p| matching(p).iter().map(|c| c.name).collect::<Vec<_>>();
        assert_eq!(names("t"), ["ticket"]);
        assert_eq!(names("q"), ["questions", "quit"]);
        assert_eq!(names("ex"), ["quit"]);
        assert_eq!(names("").len(), COMMANDS.len());
        assert!(names("zz").is_empty());
        assert_eq!(find("?").map(|c| c.name), Some("help"));
        assert_eq!(find("tick"), None);
    }

    #[test]
    fn completions_leave_room_for_arguments() {
        assert_eq!(find("ticket").unwrap().completion(), "/ticket ");
        assert_eq!(find("usage").unwrap().completion(), "/usage");
        assert_eq!(find("server").unwrap().usage(), "/server [name|address]");
    }

    #[test]
    fn amp_commands_come_first_then_config_then_chat() {
        let order = |g: Group| match g {
            Group::Amp => 0,
            Group::Config => 1,
            Group::Chat => 2,
        };
        assert!(
            COMMANDS
                .windows(2)
                .all(|w| order(w[0].group) <= order(w[1].group)),
            "grouped in order"
        );
        for g in [Group::Amp, Group::Config, Group::Chat] {
            assert!(COMMANDS.iter().any(|c| c.group == g), "{g:?}");
        }
        for name in [
            "mcp", "agents", "skills", "config", "model", "connect", "reload",
        ] {
            assert_eq!(find(name).unwrap().group, Group::Config, "/{name}");
        }
    }
}
