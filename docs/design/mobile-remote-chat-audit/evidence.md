# Mobile remote chat — viewport & worker-panel audit (#3026)

**Scope:** read-only UX audit of the remote-session web UI (`v2/ui/src/pages/Session.tsx` +
`src/components/remote/SessionParts.tsx`). No product code changed. All findings below are from
**emulation** (headless Chrome CDP device emulation against an isolated in-memory mock relay —
no real harness sessions were touched). Where emulation can mislead (virtual keyboard, real
`visualViewport` behaviour, iOS Safari chrome) it is flagged explicitly.

## Method

- Fixture: untracked Vite fixture (`v2/ui/.fixtures/`) serving the **real** `Session` page against
  `MockRelay` (the dev mock), with a stubbed `/api/me` and 0/2/4/8 fake running workers, a busy
  run, a 2-deep follow-up queue, an offline→online flip, and long/unbroken message probes.
- Capture: CDP `Emulation.setDeviceMetricsOverride` at 320/360/390/430 portrait (2× DPR, touch),
  844×390 landscape, iPhone-style safe-area insets (47/34), and a keyboard approximation
  (emulated viewport shrunk to 55% — see caveat in F2).
- Every screenshot below is the real component tree with the real design tokens (`index.css`
  "Stripe" theme), not a mockup.

## Component boundaries (stack top → bottom, `Session.tsx` + `SessionParts.tsx`)

| # | Component | File | Height at 390×844 | Notes |
|---|-----------|------|-------------------|-------|
| 1 | `Frame` header (`← Sessions`, host, `HeaderFacts`, `AmpConnectionBadge`, `PresenceBadge`, Disconnect) | Session.tsx:29 | **123px** | Wraps to 3 rows at 320–390 |
| 2 | `TicketLine` | SessionParts.tsx:390 | **34px** | Only when a ticket is current |
| 3 | `Banner` ×N (ended / version mismatch / offline / reconnecting) | SessionParts.tsx | 0–40px each | Stacked above transcript |
| 4 | Transcript scroller (`flex-1 min-h-0 overflow-y-auto`) | Session.tsx:148 | **gets the remainder** | 339px default w/ 4 workers; 0px worst case |
| 5 | `WorkerChips` roster | SessionParts.tsx:315 | **18px/worker + 30px footer, unbounded** | 4 workers → 122px; 8 → 210px |
| 6 | `WorkingLine` (busy/retry/compaction) | SessionParts.tsx:353 | 32px when busy |
| 7 | `QueueStrip` | SessionParts.tsx:287 | ~47px + 23px/queued item |
| 8 | `Composer` (label + 2-row textarea + Send/Queue + full-width Stop) | SessionParts.tsx:438 | **132px fixed** (textarea 55 + stop 48 + gaps) |
| 9 | `StatusLine` (session facts + board line) inside `pb-[env(safe-area-inset-bottom)]` | SessionParts.tsx:377 / Session.tsx:165 | **95px** with board line |

Viewport math at 390×844, 4 workers, idle: 123+34+339+122+0+0+132+95 = 845. The transcript is
the *only* flex child — every other row grows at its expense.

## Findings (evidence linked)

**F1 — Worker roster is unbounded and dominates the dock.** `WorkerChips` renders one full row
per worker (18px each at 390) plus a "watch or stop them on the laptop" footer, directly above
the composer. 4 workers: 122px (14% of viewport). 8 workers: 210px (25%), transcript shrinks to
219px. On a phone, N workers should cost ~one line, not N lines.
→ `390-workers4.png`, `390-workers8.png`

**F2 — Keyboard up, the transcript disappears entirely.** With a phone keyboard (≈55% visual
viewport ≈ 464px), a busy run and 2 queued messages: transcript height = **0px**; the composer
textarea bottom sits at the keyboard edge and the status line is pushed below the fold.
Dock order is roster → working line → queue strip → composer → stop → status, so the *least*
important rows for typing (roster, status) consume the *most* contested space.
*Emulation caveat: CDP cannot shrink only the visual viewport; this shrinks the whole emulated
viewport. `h-dvh` follows the visual viewport on iOS Safari/Chrome, so the direction of the
finding is reliable; exact pixel heights on real devices need a device pass.*
→ `390-worstcase-keyboard.png`, `390-keyboard.png`, `390-workers8-keyboard.png`

**F3 — Stop is a full-width 48px block under the composer.** It is the largest single touch
target on the screen, permanently visible, and directly below Send — a mis-tap zone on a phone.
R-M7 requires Stop to be *cheap*, not to be the biggest element on the page. It can sit inline
in the composer row (or header) at normal button height and remain one tap away.
→ `390-default.png`, `390-queued-busy.png`

**F4 — Header + ticket line cost 157px (19%) before any chat content.** At 320–390 the header
wraps to three rows (Sessions back-link / host+facts / badges+Disconnect). `TicketLine` is a
second full-width strip. On phone these should merge into one compact row (host + presence +
ticket chip), with Disconnect behind a menu.
→ `320-default.png`, `390-default.png`

**F5 — Textarea shows the UA resize grip despite `resize-none`.** In capture the bottom-right
resize handle rendered on the composer textarea (which also has `max-h-48`). On touch it is dead
weight; on desktop a drag can pin the composer at 192px and crush the transcript. Verify
`resize-none` wins over `.amp-input` UA styles in product code.
→ `390-default.png` (grip bottom-right of textarea)

**F6 — Safe-area inset is applied to the status line only** (`pb-[env(safe-area-inset-bottom)]`,
Session.tsx:165). The band that must never slide under the home indicator is the composer/Stop;
today it clears the inset only through normal document flow (status line below it). If the
status line is ever hidden/collapsed (recommendation 5), the inset must move to the dock itself.
→ `390-default.png` (34px home-indicator inset below status line)

**F7 — Autoscroll un-stick works.** Reader scrolled to top stays put when new events stream in
(scrollTop stayed 0 after a full reply; stick threshold 80px, Session.tsx). Follow-at-bottom
also works. No defect.
→ `390-scrolledup-reply.png`

**F8 — Long unbroken content wraps safely.** 400-char unbroken string: bubble scrollWidth ==
clientWidth, no document-level horizontal overflow. Tool details use `break-all`. No defect.
→ `390-longmsg.png`

**F9 — Reconnect/offline flow is legible.** Offline: presence badge flips to "Laptop offline",
composer disabled with reason, warn banner above transcript; reconnect: info banner + catch-up,
composer re-enabled. Banners stack above the transcript at 0–40px each — acceptable.
→ `390-offline-banner.png`, `390-reconnect.png`, `390-offline.png`

**F10 — Landscape (844×390) is genuinely chat-first** — header collapses to one row, roster
rows gain room. The portrait dock is the only problem space.
→ `390-landscape.png`

**F11 — No worker-detail return flow exists yet.** Chips are read-only (title attribute only);
"watch or stop them on the laptop" is the whole story. Whatever drawer/detail M2 adds must plan
a back/return target; noted so the recommendation below reserves the pattern.

## Recommendation (chat-first; compact worker count/status; on-demand drawer, not hidden status)

Priority on a phone: **(1) composer always visible & usable, (2) transcript readable/followable,
(3) worker status compact but glanceable, (4) session chrome reachable but small.**

1. **Collapse `WorkerChips` to a one-line summary above the composer:** "● 3 running · 1 blocked"
   + chevron. Tapping opens an on-demand **drawer** (bottom sheet) with the full roster rows
   (and later, M2 watch/stop actions). Status is never *hidden* — counts and worst-state colour
   stay on the summary line.
2. **Demote Stop** from full-width 48px to an inline button in the composer row (icon + label,
   42px), next to Send. Keep it always-enabled when online (R-M7).
3. **Merge header rows on <sm:** host + presence badge + Disconnect menu on one row; `TicketLine`
   becomes a chip inside the header (or scrolls with the transcript). Target ≤64px of chrome.
4. **`resize-none` on the composer textarea** (verify `.amp-input` doesn't re-enable it), rely on
   `max-h-48` auto-grow only.
5. **Move the keyboard-pressured rows:** with the visual viewport shrunk, hide `StatusLine` and
   keep the roster collapsed (a `visualViewport` resize listener toggling a class, or
   `env(keyboard-inset-height)` where supported) — the composer + transcript split what remains.
6. Keep banner stack, autoscroll stickiness, and queue strip as-is (F7–F9 pass).

## Real-device pass still owed (not done here — emulation only)

- iOS Safari: `100dvh` vs URL-bar collapse, actual keyboard/`visualViewport` offsets, rubber-band
  scroll inside the transcript scroller, `env(safe-area-inset-*)` on a notched device.
- Android Chrome: keyboard resize behaviour, back-gesture conflict with a bottom-sheet drawer.

## Shot index (all emulation, 2× DPR)

`320/360/390/430-default` widths · `390-workers4/8` roster scaling · `390-busy` working line ·
`390-queued-busy` 2-deep queue · `390-keyboard`, `390-workers8-keyboard`, `390-worstcase-keyboard`
keyboard scenarios · `390-landscape` · `390-offline(-banner)`, `390-reconnect` connectivity ·
`390-longmsg` unbroken text · `390-scrolledup-reply` autoscroll un-stick.
