//! Connection health, for the TUI header and `doctor`.

use std::{fmt, sync::Arc};

use tokio::sync::watch;

use crate::McpError;

/// The state of the MCP connection, updated after every request.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Health {
    /// The last request succeeded (a tool reporting `isError` still counts: the server answered).
    Connected,
    /// The last request failed or timed out, but the connection is still open.
    Degraded { reason: String },
    /// The server rejected our credentials: log in again (M1-3) or fix `AMP_TOKEN`.
    AuthRequired,
    /// The connection is closed; reconnect.
    Disconnected { reason: String },
}

impl Health {
    pub fn is_connected(&self) -> bool {
        matches!(self, Self::Connected)
    }

    /// The health a request outcome implies.
    pub(crate) fn after(result: Result<(), &McpError>) -> Self {
        match result {
            Ok(()) => Self::Connected,
            Err(McpError::AuthRequired { .. }) => Self::AuthRequired,
            Err(McpError::Closed) => Self::Disconnected {
                reason: "connection closed".to_owned(),
            },
            Err(e) => Self::Degraded {
                reason: e.to_string(),
            },
        }
    }
}

impl fmt::Display for Health {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Connected => f.write_str("connected"),
            Self::Degraded { reason } => write!(f, "degraded: {reason}"),
            Self::AuthRequired => f.write_str("re-login needed"),
            Self::Disconnected { reason } => write!(f, "disconnected: {reason}"),
        }
    }
}

/// Shared health cell: the client writes, any number of watchers read.
#[derive(Debug, Clone)]
pub(crate) struct HealthCell(Arc<watch::Sender<Health>>);

impl HealthCell {
    pub(crate) fn new() -> Self {
        Self(Arc::new(watch::Sender::new(Health::Connected)))
    }

    pub(crate) fn get(&self) -> Health {
        self.0.borrow().clone()
    }

    pub(crate) fn set(&self, health: Health) {
        self.0.send_if_modified(|current| {
            if *current == health {
                false
            } else {
                *current = health;
                true
            }
        });
    }

    pub(crate) fn subscribe(&self) -> watch::Receiver<Health> {
        self.0.subscribe()
    }
}
