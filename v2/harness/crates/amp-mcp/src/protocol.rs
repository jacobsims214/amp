//! Which MCP protocol era to speak (D2).

use std::{fmt, str::FromStr};

use rmcp::{model::ProtocolVersion, service::ClientLifecycleMode};

/// How the client picks the MCP protocol era.
///
/// AMP serves both eras on one endpoint. `Auto` speaks the stateless 2026-07-28 protocol and falls
/// back to the legacy `initialize` handshake (2025-11-25) when the server is older. The forced modes
/// exist for compatibility testing (`--protocol 2025-11-25` in the smoke test, M5-2's CI matrix).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum ProtocolMode {
    /// Probe `server/discover` (2026-07-28); fall back to `initialize` (2025-11-25).
    #[default]
    Auto,
    /// Only the stateless 2026-07-28 protocol; a legacy-only server is an error.
    Modern,
    /// Only the legacy `initialize` handshake at 2025-11-25.
    Legacy,
}

impl ProtocolMode {
    /// The protocol version `Modern` speaks (and `Auto` prefers).
    pub const MODERN_VERSION: ProtocolVersion = ProtocolVersion::V_2026_07_28;
    /// The protocol version `Legacy` speaks (and `Auto` falls back to).
    pub const LEGACY_VERSION: ProtocolVersion = ProtocolVersion::V_2025_11_25;

    /// The rmcp lifecycle for this mode.
    pub fn lifecycle(self) -> ClientLifecycleMode {
        match self {
            Self::Auto => ClientLifecycleMode::Auto {
                preferred_versions: vec![Self::MODERN_VERSION],
                legacy_version: Some(Self::LEGACY_VERSION),
            },
            Self::Modern => ClientLifecycleMode::Discover {
                preferred_versions: vec![Self::MODERN_VERSION],
            },
            Self::Legacy => ClientLifecycleMode::Initialize,
        }
    }

    /// The version to put in the client's `initialize` params. Only the legacy handshake reads it
    /// (Auto's fallback uses `legacy_version` instead), so pin it to the legacy version everywhere.
    pub(crate) fn initialize_version(self) -> ProtocolVersion {
        Self::LEGACY_VERSION
    }

    /// The accepted spellings, for help text.
    pub const NAMES: &'static [&'static str] = &["auto", "2026-07-28", "2025-11-25"];
}

impl fmt::Display for ProtocolMode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Auto => "auto",
            Self::Modern => "2026-07-28",
            Self::Legacy => "2025-11-25",
        })
    }
}

impl FromStr for ProtocolMode {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.trim().to_ascii_lowercase().as_str() {
            "auto" | "" => Ok(Self::Auto),
            "2026-07-28" | "modern" | "stateless" => Ok(Self::Modern),
            "2025-11-25" | "legacy" => Ok(Self::Legacy),
            other => Err(format!(
                "unknown MCP protocol {other:?} (expected one of: {})",
                Self::NAMES.join(", ")
            )),
        }
    }
}

/// The protocol era a connection ended up in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Era {
    /// 2026-07-28 and later: `server/discover`, no sessions, per-request `_meta`.
    Stateless,
    /// Before 2026-07-28: `initialize` handshake and a session.
    Legacy,
}

impl Era {
    pub(crate) fn of(version: &ProtocolVersion) -> Self {
        if version.has_initialize() {
            Self::Legacy
        } else {
            Self::Stateless
        }
    }
}

impl fmt::Display for Era {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Stateless => "stateless",
            Self::Legacy => "legacy",
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_every_spelling() {
        for (input, want) in [
            ("auto", ProtocolMode::Auto),
            ("AUTO", ProtocolMode::Auto),
            ("2026-07-28", ProtocolMode::Modern),
            ("modern", ProtocolMode::Modern),
            ("2025-11-25", ProtocolMode::Legacy),
            ("legacy", ProtocolMode::Legacy),
        ] {
            assert_eq!(input.parse::<ProtocolMode>().unwrap(), want, "{input}");
        }
        assert!("2024-11-05".parse::<ProtocolMode>().is_err());
    }

    #[test]
    fn display_round_trips() {
        for mode in [
            ProtocolMode::Auto,
            ProtocolMode::Modern,
            ProtocolMode::Legacy,
        ] {
            assert_eq!(mode.to_string().parse::<ProtocolMode>().unwrap(), mode);
        }
    }

    #[test]
    fn eras() {
        assert_eq!(Era::of(&ProtocolVersion::V_2026_07_28), Era::Stateless);
        assert_eq!(Era::of(&ProtocolVersion::V_2025_11_25), Era::Legacy);
    }
}
