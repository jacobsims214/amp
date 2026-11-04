//! Scrolling the transcript from the keyboard, and the "↓ new output" pill (#2185).
//!
//! The transcript is rpi-tui's primary [`ScrollView`] (follow-end). The wheel scrolls it 3 lines;
//! PageUp/PageDown a page (the viewport less 2 lines of overlap), Ctrl+U/Ctrl+D half a page, and
//! End (or Ctrl+End) jumps back to the latest output, which follows the stream again.
//!
//! While the transcript is scrolled up and something new is added to it (a streamed reply, a tool
//! call, a notice), [`TranscriptScroll::pill`] is [`NEW_OUTPUT`]. It is drawn centred on the
//! transcript's last row: in the terminal by rpi-tui's own jump-to-end indicator (so clicking it
//! jumps too), in [`crate::ChatView::render_frame`] by [`paint_pill`]. Reaching the bottom again,
//! by any route, takes it away.

use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU64, Ordering},
};

use rpi_tui::{LayoutBox, LayoutFrame, ScrollView, composite_tui_line, theme, visible_width};

/// The pill shown while the transcript is scrolled up and new output has arrived below.
pub const NEW_OUTPUT: &str = " ↓ new output · End ";

/// Wheel lines per notch (as before #2185).
pub const WHEEL_LINES: i32 = 3;

/// Where the transcript was when it was scrolled away from the end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Mark {
    /// [`TranscriptScroll::touch`]'s count then.
    generation: u64,
    /// The transcript's height then.
    height: usize,
}

/// A saved scroll position ([`TranscriptScroll::position`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ScrollPos {
    pub top: usize,
    pub content: usize,
    pub viewport: usize,
    pub at_end: bool,
}

/// The transcript's scroll position and whether output arrived while it was scrolled up.
/// Cloneable: the clones share their state (the terminal's pill label holds one).
#[derive(Clone)]
pub struct TranscriptScroll {
    view: Arc<ScrollView>,
    /// Bumped whenever something is added to or streamed into the transcript.
    generation: Arc<AtomicU64>,
    /// Set while scrolled up: what the transcript was when the person left the end.
    mark: Arc<Mutex<Option<Mark>>>,
}

impl TranscriptScroll {
    pub fn new(view: Arc<ScrollView>) -> Self {
        Self {
            view,
            generation: Arc::new(AtomicU64::new(0)),
            mark: Arc::new(Mutex::new(None)),
        }
    }

    /// The scroll view itself (for the layout).
    pub fn view(&self) -> &Arc<ScrollView> {
        &self.view
    }

    /// Something was added to the transcript or streamed into it.
    pub fn touch(&self) {
        self.generation.fetch_add(1, Ordering::Relaxed);
    }

    /// Whether the view is following the end (new output scrolls into view).
    pub fn at_end(&self) -> bool {
        self.view.is_following_end()
    }

    /// The first transcript line shown.
    pub fn top(&self) -> usize {
        self.view.scroll_top()
    }

    /// Lines the transcript can show at once (0 before the first layout).
    pub fn viewport(&self) -> usize {
        self.view.viewport_height()
    }

    /// Scrolls by `lines` (negative = up, towards older output).
    pub fn scroll_by(&self, lines: i32) {
        self.view.scroll_by(lines);
        self.note_position();
    }

    /// A page: the viewport less two lines, so the last lines of the old page stay in sight.
    pub fn page(&self) -> i32 {
        self.viewport().saturating_sub(2).max(1) as i32
    }

    /// Half the viewport.
    pub fn half_page(&self) -> i32 {
        (self.viewport() / 2).max(1) as i32
    }

    /// Puts transcript line `line` at the top of the view (a thinking header that opened, #2345).
    pub fn scroll_to(&self, line: usize) {
        self.view.scroll_to(line);
        self.note_position();
    }

    /// PageUp (`-1`) / PageDown (`1`).
    pub fn page_by(&self, pages: i32) {
        self.scroll_by(pages * self.page());
    }

    /// Ctrl+U (`-1`) / Ctrl+D (`1`).
    pub fn half_page_by(&self, halves: i32) {
        self.scroll_by(halves * self.half_page());
    }

    /// Where the view is now, to come back to with [`Self::restore`] (#2347: a worker's session
    /// shown in its place and closed again).
    pub fn position(&self) -> ScrollPos {
        ScrollPos {
            top: self.view.scroll_top(),
            content: self.view.content_height(),
            viewport: self.view.viewport_height(),
            at_end: self.at_end(),
        }
    }

    /// Back to `pos`. The transcript only grows, so its old top is still a valid place; at the
    /// end, it follows the end again.
    pub fn restore(&self, pos: ScrollPos) {
        if pos.at_end {
            self.to_end();
            return;
        }
        self.view.update_layout(pos.content, pos.viewport, None);
        self.view.scroll_to(pos.top);
        self.note_position();
    }

    /// End: back to the latest output, following the stream again.
    pub fn to_end(&self) {
        self.view.scroll_to_end();
        self.clear_mark();
    }

    /// [`NEW_OUTPUT`] while scrolled up with new output below, else `None`.
    pub fn pill(&self) -> Option<&'static str> {
        if self.at_end() {
            self.clear_mark();
            return None;
        }
        let mark = (*self.mark.lock().unwrap_or_else(|e| e.into_inner()))?;
        let newer = self.generation.load(Ordering::Relaxed) != mark.generation;
        (newer && self.view.content_height() > mark.height).then_some(NEW_OUTPUT)
    }

    /// The pill styled for the terminal, or `""` (nothing drawn): rpi-tui's indicator label.
    pub fn pill_label(&self) -> String {
        match self.pill() {
            Some(text) => styled(text),
            None => String::new(),
        }
    }

    /// After a scroll: leaving the end remembers where the transcript was; reaching it forgets.
    fn note_position(&self) {
        if self.at_end() {
            self.clear_mark();
            return;
        }
        let mut mark = self.mark.lock().unwrap_or_else(|e| e.into_inner());
        if mark.is_none() {
            *mark = Some(Mark {
                generation: self.generation.load(Ordering::Relaxed),
                height: self.view.content_height(),
            });
        }
    }

    fn clear_mark(&self) {
        *self.mark.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }
}

/// The pill in the theme's accent, inverted so it stands out from the transcript text.
fn styled(text: &str) -> String {
    let c = theme().colors;
    format!("\x1b[7m{}\x1b[27m", c.accent.fg(&rpi_tui::bold(text)))
}

/// Draws `pill` centred on the last row of the frame's primary scroll view, the way rpi-tui's
/// jump-to-end indicator does in the terminal. For [`crate::ChatView::render_frame`] (tests and
/// `--snapshot`); the terminal draws its own.
pub fn paint_pill(frame: &LayoutFrame, lines: &mut [String], pill: &str) {
    let Some(view) = primary_box(&frame.root) else {
        return;
    };
    let clip = view.clip;
    if clip.width == 0 || clip.height == 0 {
        return;
    }
    let row = clip.y + clip.height - 1;
    let Some(line) = lines.get_mut(row) else {
        return;
    };
    let label = styled(pill);
    let width = visible_width(&label).min(clip.width);
    let column = clip.x + (clip.width - width) / 2;
    *line = composite_tui_line(line, &label, column, width, frame.width);
}

fn primary_box(node: &LayoutBox) -> Option<&LayoutBox> {
    if node.scroll_view.as_ref().is_some_and(|s| s.is_primary()) {
        return Some(node);
    }
    node.children.iter().find_map(primary_box)
}
