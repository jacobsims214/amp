//! The chat footer (#2186): where the session is (cwd, branch, role · #ticket) and what it costs
//! (`$0.004 · ctx 4%`, the model • thinking), in place of pi's `FooterComponent`.
//!
//! ```text
//! wide (>= FOLD_BELOW columns), two rows:
//! ~/dev/harness-play (main) • manager · #2186
//! $0.004 · ctx 4%                                               mimo-v2.6-pro • high
//!
//! narrow, one row:
//! …/harness-play (main) • manager · #2186    $0.004 · ctx 4% · mimo-v2.6-pro • high
//! ```
//!
//! pi's footer spent a row on `↑547 ↓348 R117k CH99.5% $0.001 3.8%/1.1M (auto)`, which needs a
//! legend, and printed the cwd in full. Here the cwd is home-relative (`~`) and shortened from the
//! left, a whole path component at a time; the token and cache numbers moved to `/usage`
//! ([`Footer::usage`], [`Footer::context`]). Below [`FOLD_BELOW`] columns the two rows fold into
//! one, and when even that doesn't fit, the least useful pieces go first: thinking, branch, cost,
//! the model, the session name, then the cwd shrinks to `…`.

use std::{any::Any, sync::Mutex};

use rpi_tui::{
    Component,
    footer::{UsageTotals, format_cwd_for_footer},
    theme, visible_width,
};

use crate::chrome::shorten;

pub use rpi_tui::footer::format_tokens;

/// Below this many columns (the footer's own width, inside the `[tui] padding`: 96 is a
/// 100-column terminal with the default 2-column padding) the footer is one row (the AMP status
/// line above it is the other).
pub const FOLD_BELOW: usize = 96;
/// The fewest blank cells between the footer's left and right groups.
const GAP: usize = 2;

#[derive(Debug, Clone, Default)]
struct State {
    cwd: String,
    home: Option<String>,
    branch: Option<String>,
    session: Option<String>,
    model: String,
    thinking: Option<String>,
    usage: UsageTotals,
    context_tokens: Option<i64>,
    context_window: i64,
    cache_hit: Option<f64>,
    /// The remote control badge (#2376), drawn first on the first row.
    badge: Option<(String, BadgeTone)>,
}

/// How the footer's badge is drawn (#2376).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BadgeTone {
    /// Remote control is on and connected.
    On,
    /// Starting, offline or reconnecting.
    Warn,
}

/// The chat footer. Setters take effect on the next render.
#[derive(Debug)]
pub struct Footer {
    state: Mutex<State>,
}

impl Default for Footer {
    fn default() -> Self {
        Self::new()
    }
}

/// The context in use: tokens, the model's window, and the share as a percentage.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct ContextUse {
    pub tokens: i64,
    pub window: i64,
    pub percent: f64,
}

impl Footer {
    pub fn new() -> Self {
        let home = std::env::var("HOME")
            .ok()
            .or_else(|| std::env::var("USERPROFILE").ok());
        Self {
            state: Mutex::new(State {
                home,
                ..State::default()
            }),
        }
    }

    fn with<R>(&self, f: impl FnOnce(&mut State) -> R) -> R {
        f(&mut self.state.lock().unwrap_or_else(|e| e.into_inner()))
    }

    /// The home directory shown as `~` (default: `$HOME`).
    pub fn set_home(&self, home: Option<&str>) {
        self.with(|s| s.home = home.map(str::to_owned));
    }

    pub fn set_cwd(&self, cwd: &str) {
        self.with(|s| s.cwd = cwd.to_owned());
    }

    pub fn set_git_branch(&self, branch: Option<&str>) {
        self.with(|s| s.branch = branch.filter(|b| !b.is_empty()).map(str::to_owned));
    }

    /// `manager`, or `manager · #2186` while a ticket is current.
    pub fn set_session_name(&self, name: Option<&str>) {
        self.with(|s| s.session = name.filter(|n| !n.is_empty()).map(str::to_owned));
    }

    pub fn set_model(&self, model: &str) {
        self.with(|s| s.model = model.to_owned());
    }

    pub fn set_thinking_level(&self, level: Option<&str>) {
        self.with(|s| s.thinking = level.map(str::to_owned));
    }

    /// The model's context window in tokens (0 = unknown).
    pub fn set_context_window(&self, window: i64) {
        self.with(|s| s.context_window = window);
    }

    /// The latest response's context size (`None` = not known yet).
    pub fn set_context_tokens(&self, tokens: Option<i64>) {
        self.with(|s| s.context_tokens = tokens);
    }

    /// The badge before everything else on the first row (`REMOTE ON · 1 device`, #2376);
    /// `None` removes it.
    pub fn set_badge(&self, badge: Option<(String, BadgeTone)>) {
        self.with(|s| s.badge = badge);
    }

    /// The badge's text, if one is shown.
    pub fn badge(&self) -> Option<String> {
        self.with(|s| s.badge.as_ref().map(|(t, _)| t.clone()))
    }

    /// The latest prompt-cache hit rate, 0..=100.
    pub fn set_cache_hit_rate(&self, rate: Option<f64>) {
        self.with(|s| s.cache_hit = rate);
    }

    /// Adds one response's usage to the session totals.
    pub fn add_usage(&self, input: i64, output: i64, cache_read: i64, cache_write: i64, cost: f64) {
        self.with(|s| s.usage.add(input, output, cache_read, cache_write, cost));
    }

    /// The session's usage totals (what `/usage` lists).
    pub fn usage(&self) -> UsageTotals {
        self.with(|s| s.usage)
    }

    /// The latest prompt-cache hit rate, 0..=100.
    pub fn cache_hit_rate(&self) -> Option<f64> {
        self.with(|s| s.cache_hit)
    }

    /// The context in use, once a response has reported it and the window is known.
    pub fn context(&self) -> Option<ContextUse> {
        self.with(|s| match s.context_tokens {
            Some(tokens) if tokens > 0 && s.context_window > 0 => Some(ContextUse {
                tokens,
                window: s.context_window,
                percent: tokens as f64 / s.context_window as f64 * 100.0,
            }),
            _ => None,
        })
    }
}

/// `ctx 4%` (`ctx <1%` for a sliver), as `/usage` and the footer write it.
pub fn context_label(percent: f64) -> String {
    if percent > 0.0 && percent < 1.0 {
        "ctx <1%".to_owned()
    } else {
        format!("ctx {:.0}%", percent)
    }
}

/// `$0.004` (`<$0.001` for less), the session's cost.
pub fn cost_label(cost: f64) -> String {
    if cost < 0.001 {
        "<$0.001".to_owned()
    } else {
        format!("${cost:.3}")
    }
}

/// `path` shortened from the left to `max` cells: whole components first (`…/dev/harness-play`),
/// then characters (`…ness-play`).
pub fn shorten_path_left(path: &str, max: usize) -> String {
    if visible_width(path) <= max {
        return path.to_owned();
    }
    if max == 0 {
        return String::new();
    }
    // Keep the longest tail of whole components that fits after `…`.
    let mut best: Option<&str> = None;
    for (i, _) in path.match_indices('/') {
        let tail = &path[i..];
        if tail.len() > 1 && visible_width(tail) < max {
            best = Some(tail);
            break;
        }
    }
    if let Some(tail) = best {
        return format!("…{tail}");
    }
    // Not even the last component: its last characters.
    let mut kept: Vec<char> = Vec::new();
    let mut used = 0;
    for c in path.chars().rev() {
        let w = visible_width(c.encode_utf8(&mut [0; 4]));
        if used + w > max - 1 {
            break;
        }
        kept.push(c);
        used += w;
    }
    kept.reverse();
    format!("…{}", kept.into_iter().collect::<String>())
}

/// The pieces of the footer, plain; [`Pieces::left`] and [`Pieces::right`] join and style them.
struct Pieces {
    cwd: String,
    branch: Option<String>,
    session: Option<String>,
    cost: Option<String>,
    ctx: Option<(String, f64)>,
    model: Option<String>,
    thinking: Option<String>,
}

impl Pieces {
    fn from(s: &State) -> Self {
        let cwd = if s.cwd.is_empty() {
            String::new()
        } else {
            format_cwd_for_footer(&s.cwd, s.home.as_deref())
        };
        let ctx = match s.context_tokens {
            Some(t) if t > 0 && s.context_window > 0 => {
                let p = t as f64 / s.context_window as f64 * 100.0;
                Some((context_label(p), p))
            }
            _ => None,
        };
        Self {
            cwd,
            branch: s.branch.clone(),
            session: s.session.clone(),
            cost: (s.usage.cost > 0.0).then(|| cost_label(s.usage.cost)),
            ctx,
            model: (!s.model.is_empty()).then(|| s.model.clone()),
            thinking: s.thinking.as_ref().map(|t| match t.as_str() {
                "off" => "thinking off".to_owned(),
                t => t.to_owned(),
            }),
        }
    }

    /// `cwd (branch) • session`, plain.
    fn left(&self) -> String {
        let mut out = self.cwd.clone();
        if let Some(b) = &self.branch {
            if !out.is_empty() {
                out.push(' ');
            }
            out.push_str(&format!("({b})"));
        }
        if let Some(s) = &self.session {
            if !out.is_empty() {
                out.push_str(" • ");
            }
            out.push_str(s);
        }
        out
    }

    /// The usage half: `$0.004 · ctx 4%`, plain and styled.
    fn stats(&self) -> (String, String) {
        let c = theme().colors;
        let mut plain = Vec::new();
        let mut styled = Vec::new();
        if let Some(cost) = &self.cost {
            plain.push(cost.clone());
            styled.push(c.muted.fg(cost));
        }
        if let Some((label, p)) = &self.ctx {
            let color = if *p > 90.0 {
                c.error
            } else if *p > 70.0 {
                c.warning
            } else {
                c.muted
            };
            plain.push(label.clone());
            styled.push(color.fg(label));
        }
        (plain.join(" · "), styled.join(&c.dim.fg(" · ")))
    }

    /// `model • thinking`, plain.
    fn model(&self) -> String {
        match (&self.model, &self.thinking) {
            (Some(m), Some(t)) => format!("{m} • {t}"),
            (Some(m), None) => m.clone(),
            _ => String::new(),
        }
    }

    /// The narrow row's right group: stats, then the model, plain and styled.
    fn right(&self) -> (String, String) {
        let c = theme().colors;
        let (sp, ss) = self.stats();
        let model = self.model();
        match (sp.is_empty(), model.is_empty()) {
            (true, true) => (String::new(), String::new()),
            (false, true) => (sp, ss),
            (true, false) => (model.clone(), c.dim.fg(&model)),
            (false, false) => (
                format!("{sp} · {model}"),
                format!("{ss}{}{}", c.dim.fg(" · "), c.dim.fg(&model)),
            ),
        }
    }

    /// Drops the next least useful piece; false when only the cwd is left.
    fn drop_one(&mut self) -> bool {
        if self.thinking.take().is_some() || self.branch.take().is_some() {
            return true;
        }
        if self.cost.take().is_some() || self.model.take().is_some() {
            return true;
        }
        self.session.take().is_some() || self.ctx.take().is_some()
    }
}

/// `left` and right-aligned `right` in `width` cells (`left` is plain and drawn dim).
fn row(left: &str, right_plain: &str, right_styled: &str, width: usize) -> String {
    let c = theme().colors;
    let (lw, rw) = (visible_width(left), visible_width(right_plain));
    if rw == 0 {
        return c.dim.fg(&shorten(left, width));
    }
    if lw == 0 {
        let pad = width.saturating_sub(rw);
        return format!("{}{right_styled}", " ".repeat(pad));
    }
    let pad = width.saturating_sub(lw + rw);
    format!("{}{}{right_styled}", c.dim.fg(left), " ".repeat(pad))
}

/// The cwd may shrink to this many cells before other pieces are dropped.
const MIN_CWD: usize = 16;

impl Component for Footer {
    /// With a badge (#2376): the badge, then the footer in the columns left, on the first row;
    /// the badge alone when the footer would get fewer than [`MIN_CWD`] columns.
    fn render(&self, width: usize) -> Vec<String> {
        let (badge, mut state) = self.with(|s| (s.badge.clone(), s.clone()));
        let Some((text, tone)) = badge else {
            return self.render_plain(&state, width);
        };
        let c = theme().colors;
        let text = shorten(&text, width);
        let bw = visible_width(&text);
        let styled = match tone {
            BadgeTone::On => c.success.fg(&rpi_tui::bold(&text)),
            BadgeTone::Warn => c.warning.fg(&rpi_tui::bold(&text)),
        };
        let rest = width.saturating_sub(bw + GAP);
        state.badge = None;
        let lines = if rest >= MIN_CWD {
            self.render_plain(&state, rest)
        } else {
            Vec::new()
        };
        if lines.is_empty() {
            return vec![styled];
        }
        let indent = " ".repeat(bw + GAP);
        lines
            .into_iter()
            .enumerate()
            .map(|(i, line)| {
                if i == 0 {
                    format!("{styled}{}{line}", " ".repeat(GAP))
                } else {
                    format!("{indent}{line}")
                }
            })
            .collect()
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

impl Footer {
    fn render_plain(&self, state: &State, width: usize) -> Vec<String> {
        let state = state.clone();
        let mut p = Pieces::from(&state);
        if width == 0 {
            return Vec::new();
        }
        let c = theme().colors;
        // Compact (#2346): one row at any width.
        if width >= FOLD_BELOW && !crate::density::compact() {
            // Two rows: where, then what it costs and the model.
            let mut lines = Vec::new();
            let left = p.left();
            if !left.is_empty() {
                let others = visible_width(&left) - visible_width(&p.cwd);
                p.cwd = shorten_path_left(&p.cwd, width.saturating_sub(others));
                lines.push(c.dim.fg(&shorten(&p.left(), width)));
            }
            let (sp, ss) = p.stats();
            let model = p.model();
            let (sw, mw) = (visible_width(&sp), visible_width(&model));
            if sw == 0 && mw > 0 {
                let model = shorten(&model, width);
                let pad = width.saturating_sub(visible_width(&model));
                lines.push(format!("{}{}", " ".repeat(pad), c.dim.fg(&model)));
            } else if sw > 0 && sw + GAP + mw <= width {
                let pad = width - sw - mw;
                lines.push(format!("{ss}{}{}", " ".repeat(pad), c.dim.fg(&model)));
            } else if sw > 0 {
                lines.push(ss);
            }
            return lines;
        }

        // One row: shrink the cwd to MIN_CWD, then drop pieces, then shrink the cwd further.
        let fits = |p: &Pieces| {
            let (lw, rw) = (visible_width(&p.left()), visible_width(&p.right().0));
            lw + rw + if lw > 0 && rw > 0 { GAP } else { 0 } <= width
        };
        let full_cwd = p.cwd.clone();
        loop {
            p.cwd = full_cwd.clone();
            let over = |p: &Pieces| {
                let (lw, rw) = (visible_width(&p.left()), visible_width(&p.right().0));
                (lw + rw + if lw > 0 && rw > 0 { GAP } else { 0 }).saturating_sub(width)
            };
            let excess = over(&p);
            if excess > 0 {
                let cw = visible_width(&p.cwd);
                let target = cw.saturating_sub(excess).max(MIN_CWD.min(cw));
                p.cwd = shorten_path_left(&p.cwd, target);
            }
            if fits(&p) || !p.drop_one() {
                break;
            }
        }
        if !fits(&p) {
            let (lw, rw) = (visible_width(&p.left()), visible_width(&p.right().0));
            let excess = (lw + rw + GAP).saturating_sub(width);
            let cw = visible_width(&p.cwd);
            p.cwd = shorten_path_left(&p.cwd, cw.saturating_sub(excess).max(1));
        }
        let left = p.left();
        let (rp, rs) = p.right();
        if left.is_empty() && rp.is_empty() {
            // Nothing to say (no cwd, model or usage yet): no row, as the two-row layout.
            return Vec::new();
        }
        if visible_width(&left) + visible_width(&rp) + GAP > width && !rp.is_empty() {
            // Still too wide (a tiny terminal): the left side alone, cut.
            return vec![c.dim.fg(&shorten(&left, width))];
        }
        vec![row(&left, &rp, &rs, width)]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn footer() -> Footer {
        let f = Footer::new();
        f.set_home(Some("/Users/ada"));
        f.set_cwd("/Volumes/external-data/development/harness-play");
        f.set_git_branch(Some("main"));
        f.set_session_name(Some("manager · #2186"));
        f.set_model("xiaomi/mimo-v2.6-pro");
        f.set_thinking_level(Some("high"));
        f.set_context_window(1_100_000);
        f
    }

    fn plain(lines: &[String]) -> Vec<String> {
        lines
            .iter()
            .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
            .collect()
    }

    #[test]
    fn narrow_is_one_row_with_cost_and_context() {
        let f = footer();
        f.add_usage(547, 348, 117_000, 0, 0.0042);
        f.set_context_tokens(Some(44_000));
        f.set_cache_hit_rate(Some(99.5));
        let rows = plain(&f.render(80));
        assert_eq!(rows.len(), 1, "{rows:?}");
        let row = &rows[0];
        assert!(row.contains("$0.004 · ctx 4%"), "{row}");
        assert!(row.contains("…/harness-play"), "{row}");
        assert!(row.contains("manager · #2186"), "{row}");
        // pi's cryptic counters are gone.
        for gone in ["↑", "↓", "R117k", "CH", "(auto)"] {
            assert!(!row.contains(gone), "{gone} in {row}");
        }
    }

    #[test]
    fn wide_is_one_row_when_compact() {
        let f = footer();
        f.add_usage(1, 1, 0, 0, 0.0042);
        f.set_context_tokens(Some(44_000));
        let rows = plain(&f.render(120));
        assert_eq!(rows.len(), 1, "{rows:?}");
        assert!(rows[0].starts_with("/Volumes/"), "{rows:?}");
        assert!(
            rows[0].ends_with("$0.004 · ctx 4% · xiaomi/mimo-v2.6-pro • high"),
            "{rows:?}"
        );
    }

    #[test]
    fn wide_is_two_rows_when_comfortable() {
        let f = footer();
        f.add_usage(1, 1, 0, 0, 0.0042);
        f.set_context_tokens(Some(44_000));
        let rows = crate::density::with(crate::Density::Comfortable, || plain(&f.render(120)));
        assert_eq!(
            rows,
            vec![
                "/Volumes/external-data/development/harness-play (main) • manager · #2186"
                    .to_owned(),
                format!(
                    "$0.004 · ctx 4%{}xiaomi/mimo-v2.6-pro • high",
                    " ".repeat(120 - 15 - 27)
                ),
            ]
        );
    }

    #[test]
    fn home_is_a_tilde() {
        let f = footer();
        f.set_cwd("/Users/ada/dev/amp");
        let rows = plain(&f.render(80));
        assert!(
            rows[0].starts_with("~/dev/amp (main) • manager"),
            "{rows:?}"
        );
    }

    #[test]
    fn no_usage_yet_shows_no_numbers() {
        let rows = plain(&footer().render(80));
        assert!(!rows[0].contains("ctx"), "{rows:?}");
        assert!(!rows[0].contains('$'), "{rows:?}");
        assert!(rows[0].ends_with("xiaomi/mimo-v2.6-pro • high"), "{rows:?}");
    }

    #[test]
    fn every_width_fits() {
        let f = footer();
        f.add_usage(1, 1, 0, 0, 12.5);
        f.set_context_tokens(Some(1_000_000));
        for width in 1..=220 {
            for line in f.render(width) {
                assert!(
                    visible_width(&line) <= width,
                    "{width}: {:?}",
                    rpi_tui::strip_ansi(&line)
                );
            }
        }
    }

    #[test]
    fn paths_shorten_from_the_left() {
        assert_eq!(shorten_path_left("/a/bb/ccc", 20), "/a/bb/ccc");
        assert_eq!(shorten_path_left("/aaaa/bbbb/cccc", 11), "…/bbbb/cccc");
        assert_eq!(shorten_path_left("/aaaa/bbbb/cccc", 8), "…/cccc");
        assert_eq!(shorten_path_left("/aaaa/bbbb/cccccccc", 5), "…cccc");
        assert_eq!(shorten_path_left("~/x", 0), "");
    }

    #[test]
    fn labels() {
        assert_eq!(context_label(3.8), "ctx 4%");
        assert_eq!(context_label(0.2), "ctx <1%");
        assert_eq!(cost_label(0.0042), "$0.004");
        assert_eq!(cost_label(0.0001), "<$0.001");
    }
}
