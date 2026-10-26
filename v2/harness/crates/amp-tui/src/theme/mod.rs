//! The AMP theme (D10, M3-1 #2103): our JSON theme format, loaded into rpi-tui's
//! [`rpi_tui::Theme`] plus an [`AmpPalette`] for what `ThemeColors` has no role for (ticket
//! states, epic colours, the brand bands).
//!
//! - **Format** (project 2 KB `research/harness-tui-theme-and-metadata.md` §1): `colors` holds
//!   every rpi-tui role by pi's camelCase token name as `#RRGGBB` or `"default"`; `colors256` holds
//!   the xterm-256 fallbacks (an index or `"default"`; a role left out gets the nearest index);
//!   `amp` holds the AMP palette (`state`, `state256`, `glyph`, `epic`, `epic256`, `bands`,
//!   `bands256`). Unknown keys and missing roles are errors, so typos surface.
//! - **Built in:** `amp-dark` and `amp-light` (`crates/amp-tui/themes/*.json`, embedded). The
//!   colours are the provisional simstech tokens v1 (#1815 is not closed).
//! - **Depth:** [`ColorDepth::TrueColor`] uses the hex values, [`ColorDepth::Ansi256`] the
//!   `*256` values; in 256 colours the tinted tool and skill backgrounds are `default`, because
//!   they all collapse to one grey there.
//! - **Borders** are [`BorderStyle::Sharp`]: AMP is square-edged.
//! - [`install`] makes a theme current for every rpi-tui component and for [`palette`]. Picking the
//!   theme (config, env, terminal background) is [`select`].

mod color;
pub mod select;

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    path::Path,
    sync::{OnceLock, RwLock},
};

use amp_core::tickets::Status;
use rpi_tui::theme::{BorderStyle, Color, CornerStyle, Theme, ThemeColors, ThemeSpacing};
use serde::Deserialize;

pub use color::{nearest_256, parse_hex, relative_luminance, xterm_rgb};
pub use select::{ColorDepth, ThemeChoice, ThemeSettings, Variant};

/// The embedded `amp-dark` theme file.
pub const AMP_DARK_JSON: &str = include_str!("../../themes/amp-dark.json");
/// The embedded `amp-light` theme file.
pub const AMP_LIGHT_JSON: &str = include_str!("../../themes/amp-light.json");

/// A theme file that could not be loaded.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThemeError(pub String);

impl fmt::Display for ThemeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ThemeError {}

fn err<T>(msg: impl Into<String>) -> Result<T, ThemeError> {
    Err(ThemeError(msg.into()))
}

// ------------------------------------------------------------------------------------------------
// The file format
// ------------------------------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct ThemeFile {
    name: String,
    /// Free text: where the colours come from while they are provisional.
    #[serde(default)]
    provisional: Option<String>,
    colors: BTreeMap<String, String>,
    #[serde(default)]
    colors256: BTreeMap<String, Value256>,
    amp: AmpBlock,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum Value256 {
    Index(u8),
    Word(String),
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct AmpBlock {
    state: BTreeMap<String, String>,
    #[serde(default)]
    state256: BTreeMap<String, Value256>,
    glyph: BTreeMap<String, String>,
    epic: Vec<String>,
    #[serde(default)]
    epic256: Vec<Value256>,
    bands: Vec<String>,
    #[serde(default)]
    bands256: Vec<Value256>,
}

type RoleField = fn(&mut ThemeColors) -> &mut Color;

/// Every `ThemeColors` role by its theme-file token (pi's `ThemeJson` names).
const ROLES: [(&str, RoleField); 40] = [
    ("text", |c| &mut c.text),
    ("muted", |c| &mut c.muted),
    ("dim", |c| &mut c.dim),
    ("accent", |c| &mut c.accent),
    ("error", |c| &mut c.error),
    ("success", |c| &mut c.success),
    ("warning", |c| &mut c.warning),
    ("info", |c| &mut c.info),
    ("background", |c| &mut c.background),
    ("surface", |c| &mut c.surface),
    ("border", |c| &mut c.border),
    ("borderAccent", |c| &mut c.border_accent),
    ("borderMuted", |c| &mut c.border_muted),
    ("selection", |c| &mut c.selection),
    ("cursor", |c| &mut c.cursor),
    ("thinkingText", |c| &mut c.thinking_text),
    ("mdHeading", |c| &mut c.md_heading),
    ("mdLink", |c| &mut c.md_link),
    ("mdLinkUrl", |c| &mut c.md_link_url),
    ("mdCode", |c| &mut c.md_code),
    ("mdCodeBg", |c| &mut c.md_code_bg),
    ("mdCodeBlock", |c| &mut c.md_code_block),
    ("mdCodeBlockBg", |c| &mut c.md_code_block_bg),
    ("mdCodeBlockBorder", |c| &mut c.md_code_block_border),
    ("mdQuote", |c| &mut c.md_quote),
    ("mdQuoteBorder", |c| &mut c.md_quote_border),
    ("mdHr", |c| &mut c.md_hr),
    ("mdListBullet", |c| &mut c.md_list_bullet),
    ("toolPendingBg", |c| &mut c.tool_pending_bg),
    ("toolSuccessBg", |c| &mut c.tool_success_bg),
    ("toolErrorBg", |c| &mut c.tool_error_bg),
    ("toolTitle", |c| &mut c.tool_title),
    ("toolOutput", |c| &mut c.tool_output),
    ("bashMode", |c| &mut c.bash_mode),
    ("customMessageBg", |c| &mut c.custom_message_bg),
    ("customMessageLabel", |c| &mut c.custom_message_label),
    ("customMessageText", |c| &mut c.custom_message_text),
    ("toolDiffAdded", |c| &mut c.tool_diff_added),
    ("toolDiffRemoved", |c| &mut c.tool_diff_removed),
    ("toolDiffContext", |c| &mut c.tool_diff_context),
];

/// The `amp.state` keys.
const STATE_KEYS: [&str; 9] = [
    "ready", "prog", "progFill", "wait", "flag", "flagText", "onFlag", "sched", "done",
];
/// The `amp.glyph` keys.
const GLYPH_KEYS: [&str; 6] = ["ready", "prog", "wait", "flag", "sched", "done"];

/// The theme-file token names of every `ThemeColors` role, in file order.
pub fn role_names() -> impl Iterator<Item = &'static str> {
    ROLES.iter().map(|(name, _)| *name)
}

// ------------------------------------------------------------------------------------------------
// The loaded theme
// ------------------------------------------------------------------------------------------------

/// How one ticket state looks: the web UI's label and glyph in AMP's colours.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StateStyle {
    /// The web UI's label (`STATE_LABEL` in ticketState.ts): "In progress", "Question", ...
    pub label: &'static str,
    /// `□` (hollow: ready, waiting) or `■` (filled: the rest), from the theme file.
    pub glyph: String,
    /// The glyph's colour (a mark, so 3:1 is enough; in-progress uses its fill colour).
    pub mark: Color,
    /// The label's colour (text-safe).
    pub text: Color,
    /// Question only: the chip's text and background colours (white on AMP's red), drawn as
    /// ` QUESTION ` like the web UI's filled `StateTag`.
    pub chip: Option<(Color, Color)>,
}

/// The AMP colours that rpi-tui's `ThemeColors` has no role for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AmpPalette {
    pub ready: StateStyle,
    pub in_progress: StateStyle,
    pub waiting: StateStyle,
    pub question: StateStyle,
    pub scheduled: StateStyle,
    pub done: StateStyle,
    /// Epic colours (`--cat-1..6`), used by the epic's position in `amp_get_plan`. Always beside
    /// the epic's name.
    pub epic: Vec<Color>,
    /// The four brand bands (header only; they never mean a state).
    pub bands: Vec<Color>,
}

impl AmpPalette {
    /// The style for a ticket state.
    pub fn state(&self, status: Status) -> &StateStyle {
        match status {
            Status::Ready => &self.ready,
            Status::InProgress => &self.in_progress,
            Status::Waiting => &self.waiting,
            Status::Question => &self.question,
            Status::Scheduled => &self.scheduled,
            Status::Done => &self.done,
        }
    }

    /// The colour of the epic at `index` (0-based) in the plan; the palette repeats.
    pub fn epic(&self, index: usize) -> Color {
        self.epic
            .get(index % self.epic.len().max(1))
            .copied()
            .unwrap_or(Color::Default)
    }

    /// A state mark: the coloured glyph and label (`■ In progress`), or for a question the
    /// filled chip (`■ QUESTION ` in white on red).
    pub fn state_mark(&self, status: Status) -> String {
        let s = self.state(status);
        match s.chip {
            Some((fg, bg)) => format!(
                "{} {}",
                s.mark.fg(&s.glyph),
                bg.bg(&fg.fg(&format!(" {} ", s.label.to_uppercase())))
            ),
            None => format!("{} {}", s.mark.fg(&s.glyph), s.text.fg(s.label)),
        }
    }

    /// `▌ name` with the epic's colour bar.
    pub fn epic_label(&self, index: usize, name: &str) -> String {
        format!("{} {name}", self.epic(index).fg("▌"))
    }

    /// The brand signature: one `▀` run per band, `per_band` cells wide.
    pub fn brand_bands(&self, per_band: usize) -> String {
        let run = "▀".repeat(per_band);
        self.bands.iter().map(|c| c.fg(&run)).collect()
    }
}

/// A loaded AMP theme: the rpi-tui theme and the AMP palette, at one colour depth.
#[derive(Debug, Clone)]
pub struct AmpTheme {
    /// The file's `name` (`amp-dark`, `amp-light`, or a user theme's).
    pub name: String,
    /// The file's `provisional` note, if any.
    pub provisional: Option<String>,
    pub depth: ColorDepth,
    pub theme: Theme,
    pub palette: AmpPalette,
}

impl AmpTheme {
    /// One of the embedded themes.
    pub fn builtin(variant: Variant, depth: ColorDepth) -> Self {
        let json = match variant {
            Variant::Dark => AMP_DARK_JSON,
            Variant::Light => AMP_LIGHT_JSON,
        };
        Self::from_json(json, depth).expect("embedded themes are checked by the unit tests")
    }

    /// Loads a theme file.
    pub fn from_file(path: &Path, depth: ColorDepth) -> Result<Self, ThemeError> {
        let text = std::fs::read_to_string(path)
            .map_err(|e| ThemeError(format!("theme {}: {e}", path.display())))?;
        Self::from_json(&text, depth)
            .map_err(|e| ThemeError(format!("theme {}: {}", path.display(), e.0)))
    }

    /// Parses our theme format.
    pub fn from_json(json: &str, depth: ColorDepth) -> Result<Self, ThemeError> {
        let file: ThemeFile = serde_json::from_str(json).map_err(|e| ThemeError(e.to_string()))?;
        let pick = |table: &str, key: &str, hex: &str, v256: Option<&Value256>| {
            resolve(depth, hex, v256).map_err(|e| ThemeError(format!("{table}.{key}: {}", e.0)))
        };

        check_keys(
            "colors",
            file.colors.keys(),
            &role_names().collect::<Vec<_>>(),
            true,
        )?;
        check_keys(
            "colors256",
            file.colors256.keys(),
            &role_names().collect::<Vec<_>>(),
            false,
        )?;
        let mut colors = ThemeColors::default();
        for (name, field) in ROLES {
            *field(&mut colors) =
                pick("colors", name, &file.colors[name], file.colors256.get(name))?;
        }

        let amp = &file.amp;
        check_keys("amp.state", amp.state.keys(), &STATE_KEYS, true)?;
        check_keys("amp.state256", amp.state256.keys(), &STATE_KEYS, false)?;
        check_keys("amp.glyph", amp.glyph.keys(), &GLYPH_KEYS, true)?;
        let state = |key: &str| pick("amp.state", key, &amp.state[key], amp.state256.get(key));
        let glyph = |key: &str| -> Result<String, ThemeError> {
            let g = amp.glyph[key].clone();
            if g.is_empty() || g.chars().any(char::is_control) {
                return err(format!("amp.glyph.{key}: must be printable text"));
            }
            Ok(g)
        };
        let plain = |label: &'static str, key: &str| -> Result<StateStyle, ThemeError> {
            let c = state(key)?;
            Ok(StateStyle {
                label,
                glyph: glyph(key)?,
                mark: c,
                text: c,
                chip: None,
            })
        };
        let in_progress = StateStyle {
            mark: state("progFill")?,
            ..plain(Status::InProgress.label(), "prog")?
        };
        let question = StateStyle {
            label: Status::Question.label(),
            glyph: glyph("flag")?,
            mark: state("flag")?,
            text: state("flagText")?,
            chip: Some((state("onFlag")?, state("flag")?)),
        };
        let palette = AmpPalette {
            ready: plain(Status::Ready.label(), "ready")?,
            in_progress,
            waiting: plain(Status::Waiting.label(), "wait")?,
            question,
            scheduled: plain(Status::Scheduled.label(), "sched")?,
            done: plain(Status::Done.label(), "done")?,
            epic: list("amp.epic", &amp.epic, &amp.epic256, depth)?,
            bands: list("amp.bands", &amp.bands, &amp.bands256, depth)?,
        };

        Ok(Self {
            name: file.name,
            provisional: file.provisional,
            depth,
            theme: Theme {
                colors,
                spacing: ThemeSpacing::default(),
                border_style: BorderStyle::Sharp,
                corner_style: CornerStyle::Sharp,
            },
            palette,
        })
    }
}

/// Unknown keys are errors; with `required`, so are missing ones.
fn check_keys<'a>(
    table: &str,
    keys: impl Iterator<Item = &'a String>,
    known: &[&str],
    required: bool,
) -> Result<(), ThemeError> {
    let have: BTreeSet<&str> = keys.map(String::as_str).collect();
    let unknown: Vec<&str> = have
        .iter()
        .copied()
        .filter(|k| !known.contains(k))
        .collect();
    if !unknown.is_empty() {
        return err(format!("{table}: unknown key(s) {}", unknown.join(", ")));
    }
    if required {
        let missing: Vec<&str> = known
            .iter()
            .copied()
            .filter(|k| !have.contains(k))
            .collect();
        if !missing.is_empty() {
            return err(format!("{table}: missing key(s) {}", missing.join(", ")));
        }
    }
    Ok(())
}

fn list(
    table: &str,
    hexes: &[String],
    v256: &[Value256],
    depth: ColorDepth,
) -> Result<Vec<Color>, ThemeError> {
    if hexes.is_empty() {
        return err(format!("{table}: needs at least one colour"));
    }
    if !v256.is_empty() && v256.len() != hexes.len() {
        return err(format!(
            "{table}256: has {} entries, {table} has {}",
            v256.len(),
            hexes.len()
        ));
    }
    hexes
        .iter()
        .enumerate()
        .map(|(i, hex)| {
            resolve(depth, hex, v256.get(i))
                .map_err(|e| ThemeError(format!("{table}[{i}]: {}", e.0)))
        })
        .collect()
}

/// One colour at `depth`. The hex is always validated, so a theme that loads in one depth loads
/// in both.
fn resolve(depth: ColorDepth, hex: &str, v256: Option<&Value256>) -> Result<Color, ThemeError> {
    let rgb = if hex == "default" {
        None
    } else {
        Some(
            parse_hex(hex)
                .ok_or_else(|| ThemeError(format!("`{hex}` is not #RRGGBB or \"default\"")))?,
        )
    };
    let fallback = match v256 {
        None => None,
        Some(Value256::Index(i)) => Some(Color::Ansi256(*i)),
        Some(Value256::Word(w)) if w == "default" => Some(Color::Default),
        Some(Value256::Word(w)) => {
            return err(format!(
                "256-colour value `{w}` is not 0..=255 or \"default\""
            ));
        }
    };
    Ok(match (depth, rgb) {
        (ColorDepth::TrueColor, Some((r, g, b))) => Color::Rgb(r, g, b),
        (ColorDepth::TrueColor, None) => Color::Default,
        (ColorDepth::Ansi256, rgb) => fallback.unwrap_or_else(|| match rgb {
            Some(rgb) => Color::Ansi256(nearest_256(rgb)),
            None => Color::Default,
        }),
    })
}

// ------------------------------------------------------------------------------------------------
// The current theme
// ------------------------------------------------------------------------------------------------

static PALETTE: OnceLock<RwLock<AmpPalette>> = OnceLock::new();
static CURRENT: OnceLock<RwLock<Option<(String, ColorDepth)>>> = OnceLock::new();

/// Makes `theme` current: rpi-tui's global theme (which also invalidates cached renders) and
/// [`palette`]. Call it before building the shell.
pub fn install(theme: &AmpTheme) {
    rpi_tui::global_theme_manager().set(theme.theme.clone());
    let palette = PALETTE.get_or_init(|| RwLock::new(theme.palette.clone()));
    if let Ok(mut p) = palette.write() {
        *p = theme.palette.clone();
    }
    let current = CURRENT.get_or_init(|| RwLock::new(None));
    if let Ok(mut c) = current.write() {
        *c = Some((theme.name.clone(), theme.depth));
    }
}

/// The current AMP palette: the installed theme's, else `amp-dark` in 256 colours (safe in any
/// terminal).
pub fn palette() -> AmpPalette {
    PALETTE
        .get_or_init(|| RwLock::new(AmpTheme::builtin(Variant::Dark, ColorDepth::Ansi256).palette))
        .read()
        .map(|p| p.clone())
        .unwrap_or_else(|_| AmpTheme::builtin(Variant::Dark, ColorDepth::Ansi256).palette)
}

/// The installed theme's name and depth, if [`install`] has run.
pub fn current() -> Option<(String, ColorDepth)> {
    CURRENT.get()?.read().ok()?.clone()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn both() -> [AmpTheme; 4] {
        [
            AmpTheme::builtin(Variant::Dark, ColorDepth::TrueColor),
            AmpTheme::builtin(Variant::Dark, ColorDepth::Ansi256),
            AmpTheme::builtin(Variant::Light, ColorDepth::TrueColor),
            AmpTheme::builtin(Variant::Light, ColorDepth::Ansi256),
        ]
    }

    #[test]
    fn builtins_load_in_both_depths() {
        let [dt, d256, lt, l256] = both();
        assert_eq!(dt.name, "amp-dark");
        assert_eq!(lt.name, "amp-light");
        assert!(dt.provisional.as_deref().unwrap_or("").contains("#1815"));
        for t in [&dt, &d256, &lt, &l256] {
            assert!(matches!(t.theme.border_style, BorderStyle::Sharp));
            assert!(matches!(t.theme.corner_style, CornerStyle::Sharp));
            assert_eq!(t.theme.colors.background, Color::Default);
            assert_eq!(t.palette.epic.len(), 6);
            assert_eq!(t.palette.bands.len(), 4);
        }
        // Spot checks against the #2078 mapping table.
        assert_eq!(dt.theme.colors.text, Color::Rgb(0xE8, 0xEC, 0xF2));
        assert_eq!(dt.theme.colors.accent, Color::Rgb(0x7F, 0xA9, 0xE8));
        assert_eq!(dt.theme.colors.cursor, Color::Rgb(0xCF, 0x44, 0x26));
        assert_eq!(lt.theme.colors.text, Color::Rgb(0x16, 0x18, 0x1B));
        assert_eq!(lt.theme.colors.warning, Color::Rgb(0x9A, 0x5E, 0x0A));
        assert_eq!(d256.theme.colors.text, Color::Ansi256(255));
        assert_eq!(d256.theme.colors.accent, Color::Ansi256(111));
        assert_eq!(l256.theme.colors.text, Color::Ansi256(234));
        assert_eq!(l256.theme.colors.border, Color::Ansi256(102));
    }

    #[test]
    fn fallback_256_has_no_truecolor_and_drops_tinted_backgrounds() {
        for t in both().iter().filter(|t| t.depth == ColorDepth::Ansi256) {
            let c = &t.theme.colors;
            let mut all = Vec::new();
            let mut probe = c.clone();
            for (_, field) in ROLES {
                all.push(*field(&mut probe));
            }
            for s in Status::ALL {
                let st = t.palette.state(s);
                all.extend([st.mark, st.text]);
                if let Some((fg, bg)) = st.chip {
                    all.extend([fg, bg]);
                }
            }
            all.extend(t.palette.epic.iter().copied());
            all.extend(t.palette.bands.iter().copied());
            assert!(
                all.iter()
                    .all(|c| matches!(c, Color::Ansi256(_) | Color::Default)),
                "{}: {all:?}",
                t.name
            );
            for bg in [
                c.tool_pending_bg,
                c.tool_success_bg,
                c.tool_error_bg,
                c.custom_message_bg,
            ] {
                assert_eq!(bg, Color::Default, "{}", t.name);
            }
        }
    }

    #[test]
    fn palette_states_match_the_web_ui() {
        let t = AmpTheme::builtin(Variant::Dark, ColorDepth::TrueColor);
        let p = &t.palette;
        assert_eq!(p.state(Status::Ready).glyph, "□");
        assert_eq!(p.state(Status::Waiting).glyph, "□");
        assert_eq!(p.state(Status::InProgress).glyph, "■");
        assert_eq!(p.state(Status::InProgress).label, "In progress");
        // In progress: text-safe blue for the label, the st-prog fill for the glyph.
        assert_eq!(p.in_progress.text, Color::Rgb(0x7F, 0xA9, 0xE8));
        assert_eq!(p.in_progress.mark, Color::Rgb(0x3A, 0x74, 0xC8));
        // Question: white on AMP red, label text in danger-ink.
        assert_eq!(
            p.question.chip,
            Some((Color::Rgb(255, 255, 255), Color::Rgb(0xCF, 0x44, 0x26)))
        );
        assert_eq!(p.question.text, Color::Rgb(0xF0, 0x87, 0x6D));
        let chip = rpi_tui::strip_ansi(&p.state_mark(Status::Question));
        assert_eq!(chip, "■  QUESTION ");
        assert_eq!(
            rpi_tui::strip_ansi(&p.state_mark(Status::Scheduled)),
            "■ Scheduled"
        );
        // Epic colours cycle.
        assert_eq!(p.epic(0), p.epic(6));
        assert_eq!(
            rpi_tui::strip_ansi(&p.epic_label(1, "Harness")),
            "▌ Harness"
        );
        assert_eq!(rpi_tui::strip_ansi(&p.brand_bands(2)), "▀▀▀▀▀▀▀▀");
    }

    /// Explicit 256 values that deliberately differ from the CIELAB-nearest index, each with the
    /// reason: `(theme, key, index)`.
    ///
    /// amp-red `#CF4426` is nearest to 124 (`#AF0000`), which is only 2.67:1 on the dark ground
    /// `#060A0C`: under 3:1 for the Question glyph, the chip edge and the cursor. 160 (`#D70000`)
    /// is 3.68:1 there, and the chip's white text on it is 5.40:1 (M3 design review #2109,
    /// ticket #2150). amp-light keeps 124 (7.4:1 on white).
    const CONTRAST_OVERRIDES: [(&str, &str, u8); 3] = [
        ("amp-dark", "colors.borderAccent", 160),
        ("amp-dark", "colors.cursor", 160),
        ("amp-dark", "state.flag", 160),
    ];

    /// The explicit 256 values in the embedded files are the CIELAB-nearest indexes (or
    /// `default` for the tinted backgrounds), so a user theme that leaves `colors256` out gets the
    /// same treatment. The only exceptions are the documented [`CONTRAST_OVERRIDES`].
    #[test]
    fn explicit_256_values_are_the_nearest_indexes() {
        for json in [AMP_DARK_JSON, AMP_LIGHT_JSON] {
            let file: ThemeFile = serde_json::from_str(json).unwrap();
            let mut diffs = Vec::new();
            let groups = [
                ("colors", &file.colors, &file.colors256),
                ("state", &file.amp.state, &file.amp.state256),
            ];
            for (group, hexes, indexes) in groups {
                for (key, hex) in hexes {
                    let (Some(rgb), Some(Value256::Index(i))) = (parse_hex(hex), indexes.get(key))
                    else {
                        continue;
                    };
                    let path = format!("{group}.{key}");
                    let overridden = CONTRAST_OVERRIDES
                        .iter()
                        .any(|(t, k, v)| *t == file.name && *k == path && v == i);
                    if nearest_256(rgb) != *i && !overridden {
                        diffs.push(format!(
                            "{}.{path}: {hex} file {i} nearest {}",
                            file.name,
                            nearest_256(rgb)
                        ));
                    }
                }
            }
            assert!(diffs.is_empty(), "{diffs:#?}");
        }
        // Every override is still in use (a stale entry would hide a real drift).
        for (theme, path, index) in CONTRAST_OVERRIDES {
            let json = if theme == "amp-dark" {
                AMP_DARK_JSON
            } else {
                AMP_LIGHT_JSON
            };
            let file: ThemeFile = serde_json::from_str(json).unwrap();
            let (group, key) = path.split_once('.').unwrap();
            let (hexes, indexes) = match group {
                "colors" => (&file.colors, &file.colors256),
                _ => (&file.amp.state, &file.amp.state256),
            };
            assert!(
                matches!(indexes.get(key), Some(Value256::Index(i)) if *i == index),
                "{theme}.{path}: override {index} is not in the file"
            );
            let rgb = parse_hex(&hexes[key]).unwrap();
            assert_ne!(
                nearest_256(rgb),
                index,
                "{theme}.{path}: override not needed"
            );
        }
    }

    /// WCAG 2 contrast ratio of two colours.
    fn contrast(a: (u8, u8, u8), b: (u8, u8, u8)) -> f64 {
        let (la, lb) = (relative_luminance(a), relative_luminance(b));
        (la.max(lb) + 0.05) / (la.min(lb) + 0.05)
    }

    /// The RGB a terminal shows for a loaded colour (`None` for `default`).
    fn shown(c: Color) -> Option<(u8, u8, u8)> {
        match c {
            Color::Rgb(r, g, b) => Some((r, g, b)),
            Color::Ansi256(i) => Some(xterm_rgb(i)),
            _ => None,
        }
    }

    /// Roles drawn as text on the terminal ground: WCAG AA, 4.5:1.
    const TEXT_ROLES: [&str; 23] = [
        "text",
        "muted",
        "dim",
        "accent",
        "error",
        "success",
        "warning",
        "info",
        "thinkingText",
        "mdHeading",
        "mdLink",
        "mdLinkUrl",
        "mdCode",
        "mdCodeBlock",
        "mdQuote",
        "toolTitle",
        "toolOutput",
        "bashMode",
        "customMessageLabel",
        "customMessageText",
        "toolDiffAdded",
        "toolDiffRemoved",
        "toolDiffContext",
    ];
    /// Roles drawn as marks (a cursor, an accent edge): non-text contrast, 3:1.
    const MARK_ROLES: [&str; 2] = ["borderAccent", "cursor"];

    /// Every text role is >= 4.5:1 and every state/epic mark >= 3:1 against the ground (amp-dark
    /// `#060A0C`, amp-light `#FFFFFF`), in truecolor and in 256 colours; the Question chip's text
    /// is >= 4.5:1 on its red.
    #[test]
    fn colours_meet_wcag_contrast_on_the_ground() {
        let mut fails = Vec::new();
        for t in both() {
            let ground = if t.name == "amp-dark" {
                (0x06, 0x0A, 0x0C)
            } else {
                (0xFF, 0xFF, 0xFF)
            };
            let mut check = |what: String, c: Color, on: (u8, u8, u8), min: f64| {
                let Some(rgb) = shown(c) else {
                    fails.push(format!("{} {:?} {what}: no colour", t.name, t.depth));
                    return;
                };
                let ratio = contrast(rgb, on);
                if ratio < min {
                    fails.push(format!(
                        "{} {:?} {what}: {ratio:.2}:1 < {min}:1",
                        t.name, t.depth
                    ));
                }
            };
            let mut colors = t.theme.colors.clone();
            for (name, field) in ROLES {
                let min = if TEXT_ROLES.contains(&name) {
                    4.5
                } else if MARK_ROLES.contains(&name) {
                    3.0
                } else {
                    continue;
                };
                check(name.to_string(), *field(&mut colors), ground, min);
            }
            let p = &t.palette;
            for s in [
                &p.ready,
                &p.in_progress,
                &p.waiting,
                &p.question,
                &p.scheduled,
                &p.done,
            ] {
                check(format!("{} mark", s.label), s.mark, ground, 3.0);
                check(format!("{} text", s.label), s.text, ground, 4.5);
                if let Some((fg, bg)) = s.chip {
                    let bg_rgb = shown(bg).unwrap();
                    check(format!("{} chip text", s.label), fg, bg_rgb, 4.5);
                }
            }
            for (i, c) in p.epic.iter().enumerate() {
                check(format!("epic {i}"), *c, ground, 3.0);
            }
        }
        assert!(fails.is_empty(), "{fails:#?}");
    }

    /// Ticket states are told apart by colour, not only by `□`/`■` (UX review #2175 finding 12,
    /// #2187): Done is the success token, In progress the accent, Scheduled purple, Question the
    /// flag red, Waiting dim and Ready the text colour, in every theme and depth. Ready and Waiting
    /// share the hollow glyph, so they must differ by colour too.
    #[test]
    fn ticket_states_differ_by_colour() {
        for t in both() {
            let c = &t.theme.colors;
            let p = &t.palette;
            let at = format!("{} {:?}", t.name, t.depth);
            assert_eq!(p.done.mark, c.success, "{at}: Done is the success token");
            assert_eq!(p.done.text, c.success, "{at}");
            assert_eq!(p.ready.text, c.text, "{at}: Ready is ink");
            assert_eq!(p.waiting.text, c.dim, "{at}: Waiting is dim");
            assert_eq!(
                p.in_progress.text, c.accent,
                "{at}: In progress is the accent"
            );
            let styles = [
                &p.ready,
                &p.in_progress,
                &p.waiting,
                &p.question,
                &p.scheduled,
                &p.done,
            ];
            for (i, a) in styles.iter().enumerate() {
                for b in &styles[i + 1..] {
                    assert_ne!(
                        (a.mark, a.text),
                        (b.mark, b.text),
                        "{at}: {} and {} look the same",
                        a.label,
                        b.label
                    );
                    assert_ne!(a.text, b.text, "{at}: {} / {} text", a.label, b.label);
                }
            }
        }
    }

    #[test]
    fn missing_256_values_are_derived() {
        let mut v: serde_json::Value = serde_json::from_str(AMP_DARK_JSON).unwrap();
        v.as_object_mut().unwrap().remove("colors256");
        v["amp"].as_object_mut().unwrap().remove("state256");
        let t = AmpTheme::from_json(&v.to_string(), ColorDepth::Ansi256).unwrap();
        assert_eq!(t.theme.colors.text, Color::Ansi256(255));
        assert_eq!(t.theme.colors.cursor, Color::Ansi256(124));
        assert_eq!(t.theme.colors.background, Color::Default);
        assert!(matches!(t.palette.ready.mark, Color::Ansi256(_)));
    }

    #[test]
    fn bad_files_are_errors() {
        let base: serde_json::Value = serde_json::from_str(AMP_DARK_JSON).unwrap();
        let load =
            |v: &serde_json::Value| AmpTheme::from_json(&v.to_string(), ColorDepth::TrueColor);

        let mut v = base.clone();
        v["colors"].as_object_mut().unwrap().remove("mdHr");
        assert!(load(&v).unwrap_err().0.contains("missing key(s) mdHr"));

        let mut v = base.clone();
        v["colors"]["mdHeadng"] = "#FFFFFF".into();
        assert!(load(&v).unwrap_err().0.contains("unknown key(s) mdHeadng"));

        let mut v = base.clone();
        v["colors"]["text"] = "white".into();
        assert!(load(&v).unwrap_err().0.contains("colors.text"));

        let mut v = base.clone();
        v["colors256"]["text"] = "bright".into();
        assert!(
            AmpTheme::from_json(&v.to_string(), ColorDepth::Ansi256)
                .unwrap_err()
                .0
                .contains("colors.text")
        );

        let mut v = base.clone();
        v["amp"]["epic256"] = serde_json::json!([1, 2]);
        assert!(load(&v).unwrap_err().0.contains("amp.epic256"));

        let mut v = base.clone();
        v["amp"]["glyph"].as_object_mut().unwrap().remove("flag");
        assert!(load(&v).unwrap_err().0.contains("amp.glyph"));

        let mut v = base.clone();
        v["extra"] = 1.into();
        assert!(load(&v).is_err());

        assert!(AmpTheme::from_json("{", ColorDepth::TrueColor).is_err());
    }

    #[test]
    fn from_file_names_the_path() {
        let e = AmpTheme::from_file(
            Path::new("/nonexistent/amp-theme.json"),
            ColorDepth::TrueColor,
        )
        .unwrap_err();
        assert!(e.0.contains("/nonexistent/amp-theme.json"), "{e}");
    }
}
