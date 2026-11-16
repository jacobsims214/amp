//! `/remote` in the chat (#2376): the person's side of remote control (protocol.md, KB
//! decisions/harness-remote-control.md section 3). The link itself is #2373's
//! [`amp_core::remote::RemoteLink`]; this module draws it.
//!
//! - `/remote` (or `/remote on`) turns it on and prints the pairing link, a QR code of it
//!   ([`QrBlock`], [`qr_fits`]: always when the window is wide enough for it, whatever its
//!   height — the transcript scrolls; a window too narrow for the code gets the link alone with
//!   a line saying so, #4901) and when it ends.
//!   `/remote off` ends it, `/remote pair` makes a new link, `/remote devices` lists the paired
//!   devices and `/remote devices revoke <n|all>` unpins them.
//! - The link is copied to the clipboard as it is shown (#2423, [`crate::clipboard`]): OSC 52 and
//!   `pbcopy`/`wl-copy`/`xclip`. `/remote copy` copies it again, `/remote open` opens it in this
//!   computer's browser, and its rows start at the left edge so a selection of them is the link.
//! - The footer's badge ([`badge`]): `REMOTE ON · 1 device`, `REMOTE OFFLINE · reconnecting`,
//!   `· 2 queued` while remote prompts wait.
//! - The pairing question: `Allow "Safari on iPhone" (jsims@…) to control this session? code
//!   1234-5678 [y/N]`. Only this keyboard answers it ([`crate::Shell::key`]); `y` allows, any
//!   other key (Enter too) refuses, and so does no answer before it expires.
//! - The link's notices (timeouts, Disconnect, revoke, login problems) go in the transcript as
//!   they come, and so do the losing and regaining of the connection.
//! - Remote prompts show tagged `[remote: <device>]` ([`crate::view::ChatView::add_user_tagged`]);
//!   one that has to wait for the running reply shows as queued.

use std::{any::Any, collections::BTreeSet};

use amp_core::{
    chat_event::{ChatEvent, NoticeLevel, Origin, QueueItem},
    manager_feed::FeedEntry,
    remote::{
        LocalEvent, PairingPrompt, RemoteLink, RemoteState,
        pairing::{StoredPin, sanitize_for_terminal},
    },
};
use chrono::TimeZone;
use qrcodegen::{QrCode, QrCodeEcc};
use rpi_tui::{Component, theme};
use tokio::sync::{broadcast, mpsc, watch};

use crate::footer::BadgeTone;

/// Blank modules around the QR code (the spec asks for 4; 2 scans fine on a terminal's dark
/// background because the border is drawn light).
const QUIET: i32 = 2;
/// Rows the rest of the screen needs next to the QR code: the lines around it, the editor and
/// the footer. A window with fewer rows than this plus the code's own shows only part of it, so
/// the code is still drawn (#4901: the transcript scrolls) and a line says to scroll or to make
/// the window taller.
const QR_ROOM: usize = 12;
/// Columns a QR block takes beside the code's own: its one-column left margin, and the
/// Conversation box's two edges with the blank column inside each.
const QR_SIDE_ROOM: usize = 5;

/// What `/remote` can do in this chat.
pub(crate) struct RemoteUi {
    /// The link, or why remote control can't be used from here (a switch, the login, http).
    pub(crate) link: Result<RemoteLink, String>,
    /// The warning to show before turning it on (the loopback dev override).
    pub(crate) warning: Option<String>,
    state: Option<watch::Receiver<RemoteState>>,
    local: Option<mpsc::UnboundedReceiver<LocalEvent>>,
    /// The chat feed while remote control is on, for the follow-up queue.
    queue_rx: Option<broadcast::Receiver<FeedEntry>>,
    /// The queued prompt ids already shown.
    queued: BTreeSet<String>,
    /// The pairing question waiting for its local answer.
    pub(crate) pending: Option<PairingPrompt>,
    /// The state last drawn.
    last: RemoteState,
    /// A `/remote` command's call is running.
    pub(crate) busy: bool,
    /// The chat said the connection was lost (so it says when it is back).
    lost: bool,
}

/// What the link's events mean for the chat (applied by the shell).
pub(crate) enum UiEvent {
    Line(NoticeLevel, String),
    Pairing,
    Badge(Option<(String, BadgeTone)>),
}

impl RemoteUi {
    pub(crate) fn new(link: Result<RemoteLink, String>, warning: Option<String>) -> Self {
        let (state, local) = match &link {
            Ok(l) => (Some(l.state()), l.take_local_events()),
            Err(_) => (None, None),
        };
        Self {
            link,
            warning,
            state,
            local,
            queue_rx: None,
            queued: BTreeSet::new(),
            pending: None,
            last: RemoteState::Off { reason: None },
            busy: false,
            lost: false,
        }
    }

    pub(crate) fn current(&self) -> RemoteState {
        match &self.link {
            Ok(l) => l.current(),
            Err(_) => RemoteState::Off { reason: None },
        }
    }

    /// Starts reading the follow-up queue from `feed` (when remote control turns on).
    pub(crate) fn watch_queue(
        &mut self,
        feed: Option<&std::sync::Arc<amp_core::manager_feed::ManagerFeed>>,
    ) {
        self.queue_rx = feed.map(|f| f.subscribe());
        self.queued.clear();
    }

    /// Takes everything the link said since the last call, in order.
    pub(crate) fn drain(
        &mut self,
        now_ms: i64,
        devices: &dyn Fn() -> Vec<StoredPin>,
    ) -> Vec<UiEvent> {
        let mut out = Vec::new();
        if let Some(rx) = &mut self.local {
            while let Ok(ev) = rx.try_recv() {
                match ev {
                    LocalEvent::Notice { level, text } => out.push(UiEvent::Line(level, text)),
                    LocalEvent::Pairing(prompt) => {
                        if let Some(old) = self.pending.take() {
                            // A newer request replaces it; the older one is refused.
                            let _ = old.answer.send(false);
                            out.push(UiEvent::Line(
                                NoticeLevel::Info,
                                format!(
                                    "Pairing request from \"{}\" refused: a newer one came in.",
                                    old.device_name
                                ),
                            ));
                        }
                        self.pending = Some(prompt);
                        out.push(UiEvent::Pairing);
                    }
                }
            }
        }
        if let Some(p) = &self.pending
            && now_ms >= p.expires_at_ms
            && let Some(p) = self.pending.take()
        {
            let _ = p.answer.send(false);
            out.push(UiEvent::Line(
                NoticeLevel::Info,
                format!(
                    "Pairing request from \"{}\" expired without an answer: refused.",
                    p.device_name
                ),
            ));
        }
        if let Some(rx) = &mut self.state
            && rx.has_changed().unwrap_or(false)
        {
            let now = rx.borrow_and_update().clone();
            out.extend(self.transition(&now));
            out.push(UiEvent::Badge(badge(&now)));
            self.last = now;
        }
        if let Some(rx) = &mut self.queue_rx {
            loop {
                match rx.try_recv() {
                    Ok(entry) => {
                        if let ChatEvent::Queue { items } = entry.event {
                            out.extend(queue_lines(&mut self.queued, &items, &devices()));
                        }
                    }
                    Err(broadcast::error::TryRecvError::Lagged(_)) => continue,
                    Err(_) => break,
                }
            }
        }
        out
    }

    /// Lines for a state change the link doesn't announce itself (it does say why it ended).
    fn transition(&mut self, now: &RemoteState) -> Vec<UiEvent> {
        let mut out = Vec::new();
        match (&self.last, now) {
            (RemoteState::On(a), RemoteState::On(b)) if a.online && !b.online => {
                self.lost = true;
                out.push(UiEvent::Line(
                    NoticeLevel::Warn,
                    "Remote control lost its connection to AMP; reconnecting. Paired devices see \
                     the laptop as offline meanwhile."
                        .to_owned(),
                ));
            }
            (RemoteState::On(a), RemoteState::On(b)) if !a.online && b.online && self.lost => {
                self.lost = false;
                out.push(UiEvent::Line(
                    NoticeLevel::Info,
                    "Remote control reconnected.".to_owned(),
                ));
            }
            (RemoteState::On(_), RemoteState::Off { .. }) => {
                self.lost = false;
                self.queue_rx = None;
                self.queued.clear();
                if let Some(p) = self.pending.take() {
                    let _ = p.answer.send(false);
                }
            }
            _ => {}
        }
        out
    }
}

/// The footer badge for `state`: `None` while off.
pub fn badge(state: &RemoteState) -> Option<(String, BadgeTone)> {
    match state {
        RemoteState::Off { .. } => None,
        RemoteState::Starting => Some(("REMOTE starting…".to_owned(), BadgeTone::Warn)),
        RemoteState::On(on) => {
            let mut text = if on.online {
                format!(
                    "REMOTE ON · {} device{}",
                    on.viewers,
                    if on.viewers == 1 { "" } else { "s" }
                )
            } else {
                "REMOTE OFFLINE · reconnecting".to_owned()
            };
            if on.queue_len > 0 {
                text.push_str(&format!(" · {} queued", on.queue_len));
            }
            let tone = if on.online {
                BadgeTone::On
            } else {
                BadgeTone::Warn
            };
            Some((text, tone))
        }
    }
}

/// A transcript line for each remote prompt that joined the queue.
fn queue_lines(
    seen: &mut BTreeSet<String>,
    items: &[QueueItem],
    devices: &[StoredPin],
) -> Vec<UiEvent> {
    let mut out = Vec::new();
    let ids: BTreeSet<String> = items.iter().map(|i| i.prompt_id.clone()).collect();
    for (pos, item) in items.iter().enumerate() {
        if seen.contains(&item.prompt_id) || item.origin != Origin::Remote {
            continue;
        }
        let who = item
            .device_id
            .as_deref()
            .and_then(|id| devices.iter().find(|d| d.device_id == id))
            .map_or_else(|| "a paired device".to_owned(), |d| d.name.clone());
        // #2391 (L2): the preview is remote text; nothing in it may reach the terminal raw.
        out.push(UiEvent::Line(
            NoticeLevel::Info,
            format!(
                "[remote: {}] queued #{} · runs after this reply: \"{}\" (Esc stops the reply \
                 and drops the queue)",
                sanitize_for_terminal(&who, 64),
                pos + 1,
                sanitize_for_terminal(&item.preview.replace(['\n', '\t'], " "), 400)
            ),
        ));
    }
    *seen = ids;
    out
}

/// `14:05`, or `Oct 26 02:05` when it isn't today, in local time.
pub fn clock(ms: i64, now_ms: i64) -> String {
    let (Some(at), Some(now)) = (
        chrono::Local.timestamp_millis_opt(ms).single(),
        chrono::Local.timestamp_millis_opt(now_ms).single(),
    ) else {
        return String::from("?");
    };
    if at.date_naive() == now.date_naive() {
        at.format("%H:%M").to_string()
    } else {
        at.format("%b %-d %H:%M").to_string()
    }
}

/// `5 min`, `12 h`, `30 days`, roughly.
pub fn span(ms: i64) -> String {
    let s = (ms.max(0) + 500) / 1000;
    if s >= 2 * 86_400 {
        format!("{} days", (s + 43_200) / 86_400)
    } else if s >= 3600 {
        format!("{} h", (s + 1800) / 3600)
    } else if s >= 60 {
        format!("{} min", (s + 30) / 60)
    } else {
        format!("{s} s")
    }
}

/// The `/remote devices` list: one line per pinned device, newest first.
pub fn device_lines(devices: &[StoredPin], now_ms: i64) -> Vec<String> {
    if devices.is_empty() {
        return vec![
            "No paired devices. Open the link from /remote (or /remote pair) on the device, and \
             answer y here."
                .to_owned(),
        ];
    }
    let mut lines = vec![format!(
        "Paired devices ({}): /remote devices revoke <n|all> unpins them.",
        devices.len()
    )];
    for (i, d) in sorted(devices).iter().enumerate() {
        let until = if d.session_only {
            "this session only".to_owned()
        } else if d.pinned_until_ms <= now_ms {
            "expired: pair again".to_owned()
        } else {
            format!("trusted until {}", clock(d.pinned_until_ms, now_ms))
        };
        lines.push(format!(
            "  {}. {} · paired {} · {} · {}",
            i + 1,
            d.name,
            clock(d.paired_at_ms, now_ms),
            until,
            short_id(&d.device_id)
        ));
    }
    lines
}

/// The pins in `/remote devices`'s order (newest first), so `revoke <n>` means what was listed.
pub fn sorted(devices: &[StoredPin]) -> Vec<StoredPin> {
    let mut v = devices.to_vec();
    v.sort_by(|a, b| {
        b.paired_at_ms
            .cmp(&a.paired_at_ms)
            .then(a.device_id.cmp(&b.device_id))
    });
    v
}

fn short_id(id: &str) -> String {
    let chars: Vec<char> = id.chars().collect();
    if chars.len() <= 12 {
        id.to_owned()
    } else {
        format!("{}…", chars[..10].iter().collect::<String>())
    }
}

/// Which pin `/remote devices revoke <which>` names: a list number, or (the start of) an id.
pub fn pick<'a>(devices: &'a [StoredPin], which: &str) -> Result<&'a StoredPin, String> {
    if let Ok(n) = which.parse::<usize>() {
        return devices
            .get(n.wrapping_sub(1))
            .ok_or_else(|| format!("there is no device {n} (see /remote devices)"));
    }
    let which = which.trim_end_matches('…');
    let found: Vec<&StoredPin> = devices
        .iter()
        .filter(|d| !which.is_empty() && d.device_id.starts_with(which))
        .collect();
    match found.as_slice() {
        [one] => Ok(one),
        [] => Err(format!("no paired device `{which}` (see /remote devices)")),
        _ => Err(format!(
            "`{which}` matches more than one device; use its number"
        )),
    }
}

/// The QR code of `link`: (columns, rows) it takes in the terminal, quiet zone included.
pub fn qr_size(link: &str) -> Option<(usize, usize)> {
    let qr = QrCode::encode_text(link, QrCodeEcc::Low).ok()?;
    let side = (qr.size() + 2 * QUIET) as usize;
    Some((side, side.div_ceil(2)))
}

/// The window the QR code of `link` wants in order to be seen whole: (columns, rows), the code
/// plus the room it needs around it ([`QR_SIDE_ROOM`] beside it, [`QR_ROOM`] for the rest of
/// the screen).
pub fn qr_window(link: &str) -> Option<(usize, usize)> {
    qr_size(link).map(|(cols, rows)| (cols + QR_SIDE_ROOM, rows + QR_ROOM))
}

/// Whether the QR code of `link` can be drawn in a `width`-wide window, at any height.
///
/// #4901: only the width decides. A real link's code is about 25 rows tall, which no 80x24
/// terminal has room for beside the editor and the footer, and withholding it left a phone with
/// nothing to scan. The code goes in the transcript, which scrolls, so it is drawn whatever the
/// window's height; when the window is too short to show all of it at once the chat says how
/// tall it would have to be ([`qr_window`]) and the link is still there to copy. A window too
/// narrow for the code gets the link alone, with a line saying so.
pub fn qr_fits(link: &str, width: usize) -> bool {
    qr_size(link).is_some_and(|(cols, _)| cols + QR_SIDE_ROOM <= width)
}

/// The pairing link on its own rows (#2376): cut at the width (a link has no spaces to wrap
/// at), each piece an OSC 8 hyperlink to the whole link, so a click on any row opens all of it
/// in terminals that support hyperlinks. Never sent to the manager feed.
///
/// #2423: the rows start at the screen's left edge ([`crate::inset::FLUSH_LEFT`]: no padding,
/// no leading space), so a terminal selection from the first character to the last is the link
/// plus the line breaks, which browsers drop from a pasted address.
pub struct LinkBlock {
    url: String,
}

impl LinkBlock {
    pub fn new(url: &str) -> Self {
        // A link is ASCII; anything else (a control character) is dropped.
        Self {
            url: url.chars().filter(|c| c.is_ascii_graphic()).collect(),
        }
    }
}

impl Component for LinkBlock {
    fn render(&self, width: usize) -> Vec<String> {
        let room = width.max(1);
        let chars: Vec<char> = self.url.chars().collect();
        let accent = theme().colors.accent;
        let mut out = vec![String::new()];
        for piece in chars.chunks(room) {
            let piece: String = piece.iter().collect();
            out.push(format!(
                "{}\x1b]8;;{}\x07{}\x1b]8;;\x07",
                crate::inset::FLUSH_LEFT,
                self.url,
                accent.fg(&piece)
            ));
        }
        out
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The QR code of a pairing link, two modules per row (half blocks). Light modules are drawn as
/// blocks, so the text alone (no colours) still shows the code; with colours, white on black, so
/// it scans on a light theme too.
pub struct QrBlock {
    rows: Vec<String>,
    width: usize,
}

impl QrBlock {
    pub fn new(link: &str) -> Option<Self> {
        let qr = QrCode::encode_text(link, QrCodeEcc::Low).ok()?;
        let n = qr.size();
        let light = |x: i32, y: i32| -> bool {
            if x < 0 || y < 0 || x >= n || y >= n {
                true
            } else {
                !qr.get_module(x, y)
            }
        };
        let mut rows = Vec::new();
        let mut y = -QUIET;
        while y < n + QUIET {
            let mut row = String::new();
            for x in -QUIET..n + QUIET {
                let top = light(x, y);
                let bottom = if y + 1 < n + QUIET {
                    light(x, y + 1)
                } else {
                    false
                };
                row.push(match (top, bottom) {
                    (true, true) => '█',
                    (true, false) => '▀',
                    (false, true) => '▄',
                    (false, false) => ' ',
                });
            }
            rows.push(row);
            y += 2;
        }
        let width = (n + 2 * QUIET) as usize;
        Some(Self { rows, width })
    }

    pub fn rows(&self) -> &[String] {
        &self.rows
    }
}

/// What a window too narrow for the code gets instead (cut to the width it has).
const TOO_NARROW: &str = "(the QR code needs a wider window; use the link above)";

impl Component for QrBlock {
    fn render(&self, width: usize) -> Vec<String> {
        // #4901: never a row wider than the window. The code's rows carry a one-column margin,
        // so they need `self.width + 1` columns; anything less (the window shrank) gets the line
        // that says why, itself cut to the width there is.
        if width < self.width + 1 {
            let dim = theme().colors.dim;
            return vec![dim.fg(&rpi_tui::truncate_to_width(TOO_NARROW, width, ""))];
        }
        let mut out = Vec::with_capacity(self.rows.len() + 1);
        for row in &self.rows {
            // Bright white on black: the code's own colours, whatever the theme.
            out.push(format!(" \x1b[97;40m{row}\x1b[0m"));
        }
        out.push(String::new());
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
    use amp_core::remote::RemoteOn;

    fn on(viewers: u32, online: bool, queue_len: usize) -> RemoteState {
        RemoteState::On(RemoteOn {
            session_id: "rs_1".into(),
            link: None,
            link_expires_at_ms: None,
            viewers,
            devices: 1,
            online,
            expires_at_ms: None,
            idle_deadline_ms: None,
            queue_len,
            fingerprint: "AAAA-BBBB".into(),
        })
    }

    /// #2391 (L2): a queued remote prompt's preview is shown without escape sequences,
    /// invisible or bidi characters, on one line.
    #[test]
    fn queued_preview_is_sanitized() {
        let pin = StoredPin {
            device_id: "dv_1".into(),
            device_pub: String::new(),
            name: "Pho\u{200B}ne".into(),
            user_id: 7,
            paired_at_ms: 1,
            pinned_until_ms: 2,
            session_only: false,
        };
        let item = QueueItem {
            prompt_id: "p_1".into(),
            origin: Origin::Remote,
            device_id: Some("dv_1".into()),
            preview: "hi\u{1b}]52;c;Zm9v\u{7} there\u{202E}\u{2028}\nnext\u{1b}[2J".into(),
        };
        let mut seen = BTreeSet::new();
        let lines = queue_lines(&mut seen, &[item], &[pin]);
        let [UiEvent::Line(_, line)] = lines.as_slice() else {
            panic!("one line");
        };
        assert_eq!(
            line,
            "[remote: Phone] queued #1 · runs after this reply: \"hi]52;c;Zm9v there next[2J\" \
             (Esc stops the reply and drops the queue)"
        );
    }

    #[test]
    fn badges() {
        assert_eq!(badge(&RemoteState::Off { reason: None }), None);
        assert_eq!(badge(&on(0, true, 0)).unwrap().0, "REMOTE ON · 0 devices");
        assert_eq!(badge(&on(1, true, 0)).unwrap().0, "REMOTE ON · 1 device");
        assert_eq!(
            badge(&on(2, true, 2)).unwrap(),
            ("REMOTE ON · 2 devices · 2 queued".into(), BadgeTone::On)
        );
        assert_eq!(
            badge(&on(1, false, 0)).unwrap(),
            ("REMOTE OFFLINE · reconnecting".into(), BadgeTone::Warn)
        );
    }

    /// #4901: the code of a real link is drawn at 80x24 and every wider window; only a window
    /// too narrow for it gets the link alone.
    #[test]
    fn the_qr_code_of_a_real_link_fits_an_80x24_window() {
        let link = "https://amp.local/sessions/rs_01JABCDEFGHJKMNPQRSTVWXYZ0#p=\
                    Zm9vYmFyYmF6cXV4cXV1eGZvb2JhcmJhenF1eHF1dXhmb29i&h=hk_0123456789abcdef";
        let (cols, rows) = qr_size(link).unwrap();
        assert!(cols <= 60 && rows <= 30, "{cols}x{rows}");
        assert!(qr_fits(link, 80), "80x24 gets the code");
        assert!(qr_fits(link, 120));
        assert!(
            !qr_fits(link, cols + QR_SIDE_ROOM - 1),
            "one column short of the room it needs is too narrow"
        );
        assert!(qr_fits(link, cols + QR_SIDE_ROOM));
        // The window it would need to show whole (a line says so when the real one is shorter).
        assert_eq!(qr_window(link), Some((cols + QR_SIDE_ROOM, rows + QR_ROOM)));
        let block = QrBlock::new(link).unwrap();
        assert_eq!(block.rows().len(), rows);
        assert!(block.rows().iter().all(|r| r.chars().count() == cols));
        // Quiet zone: the first row is all light.
        assert!(block.rows()[0].chars().all(|c| c == '█'));
        // A narrow render says why there is no code.
        assert_eq!(block.render(cols - 1).len(), 1);
    }

    /// #4901: whatever width it is drawn at, a QR block's rows never exceed it: the code's rows
    /// carry a one-column margin and a narrower window gets a line cut to fit.
    #[test]
    fn the_qr_block_never_draws_past_the_width_it_is_given() {
        let link = "https://amp.local/sessions/rs_01JABCDEFGHJKMNPQRSTVWXYZ0#p=\
                    Zm9vYmFyYmF6cXV4cXV1eGZvb2JhcmJhenF1eHF1dXhmb29i&h=hk_0123456789abcdef";
        let (cols, rows) = qr_size(link).unwrap();
        let block = QrBlock::new(link).unwrap();
        for width in 0..=cols + 8 {
            for row in block.render(width) {
                let w = rpi_tui::visible_width(&row);
                assert!(w <= width, "{w} columns in a {width}-wide window: {row:?}");
            }
        }
        // The whole code, with its one-column margin, at the narrowest width that has room.
        let drawn = block.render(cols + 1);
        assert_eq!(drawn.len(), rows + 1, "the code and its blank row");
        assert!(
            drawn[..rows]
                .iter()
                .all(|r| rpi_tui::visible_width(r) == cols + 1)
        );
        // One column less: the line that says why, cut to the width there is.
        let narrow = block.render(cols);
        assert_eq!(narrow.len(), 1);
        assert!(narrow[0].contains("wider window"), "{}", narrow[0]);
    }

    #[test]
    fn spans_and_picks() {
        assert_eq!(span(5 * 60_000), "5 min");
        assert_eq!(span(12 * 3_600_000), "12 h");
        assert_eq!(span(30 * 86_400_000), "30 days");
        let pin = |id: &str, at: i64| StoredPin {
            device_id: id.into(),
            device_pub: String::new(),
            name: format!("dev {id}"),
            user_id: 7,
            paired_at_ms: at,
            pinned_until_ms: at + 1000,
            session_only: false,
        };
        let pins = sorted(&[pin("dv_aaa", 1), pin("dv_bbb", 2)]);
        assert_eq!(pins[0].device_id, "dv_bbb", "newest first");
        assert_eq!(pick(&pins, "1").unwrap().device_id, "dv_bbb");
        assert_eq!(pick(&pins, "dv_a").unwrap().device_id, "dv_aaa");
        assert!(pick(&pins, "3").is_err());
        assert!(pick(&pins, "0").is_err());
        assert!(pick(&pins, "dv_").is_err());
        let lines = device_lines(&pins, 1500);
        assert!(lines[1].contains("1. dev dv_bbb"), "{lines:?}");
        assert!(lines[2].contains("expired"), "{lines:?}");
    }
}
