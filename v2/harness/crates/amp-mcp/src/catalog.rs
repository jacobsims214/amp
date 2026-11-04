//! The AMP tool catalog: the `tools/list` result, kept for as long as the server's cache hint allows.
//!
//! AMP marks `tools/list` fresh for 5 minutes, scoped private (SEP-2549, `ttlMs` + `cacheScope`;
//! the list depends on who is asking). The catalog honours whatever the server sends, capped at 24 h.
//! A server that sends no hint (the legacy era may not) gets [`crate::ClientOptions::catalog_default_ttl`].
//! A `notifications/tools/list_changed` from the server makes the catalog stale at once.

use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use rmcp::model::{CacheScope, Tool};

/// Longest TTL honoured, whatever the server says (same cap as rmcp's own cache).
pub const MAX_CATALOG_TTL: Duration = rmcp::service::MAX_CLIENT_CACHE_TTL;

/// Where a catalog's TTL came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TtlSource {
    /// The server's `ttlMs` hint (the smallest across pages).
    Server,
    /// No hint: the client's configured default.
    Default,
}

/// One fetched tool list. Cheap to clone.
#[derive(Debug, Clone)]
pub struct Catalog {
    tools: Arc<Vec<Tool>>,
    fetched_at: Instant,
    ttl: Duration,
    ttl_source: TtlSource,
    cache_scope: Option<CacheScope>,
}

impl Catalog {
    pub(crate) fn new(
        tools: Vec<Tool>,
        fetched_at: Instant,
        hint_ttl_ms: Option<u64>,
        cache_scope: Option<CacheScope>,
        default_ttl: Duration,
    ) -> Self {
        let (ttl, ttl_source) = match hint_ttl_ms {
            Some(ms) => (Duration::from_millis(ms), TtlSource::Server),
            None => (default_ttl, TtlSource::Default),
        };
        Self {
            tools: Arc::new(tools),
            fetched_at,
            ttl: ttl.min(MAX_CATALOG_TTL),
            ttl_source,
            cache_scope,
        }
    }

    /// Every tool, in server order.
    pub fn tools(&self) -> &Arc<Vec<Tool>> {
        &self.tools
    }

    /// A tool by its exact AMP name.
    pub fn get(&self, name: &str) -> Option<&Tool> {
        self.tools.iter().find(|tool| tool.name == name)
    }

    /// Tool names, in server order.
    pub fn names(&self) -> impl Iterator<Item = &str> {
        self.tools.iter().map(|tool| tool.name.as_ref())
    }

    pub fn len(&self) -> usize {
        self.tools.len()
    }

    pub fn is_empty(&self) -> bool {
        self.tools.is_empty()
    }

    /// How long this list may be reused.
    pub fn ttl(&self) -> Duration {
        self.ttl
    }

    pub fn ttl_source(&self) -> TtlSource {
        self.ttl_source
    }

    /// The server's `cacheScope`, when it sent one.
    pub fn cache_scope(&self) -> Option<CacheScope> {
        self.cache_scope
    }

    pub fn fetched_at(&self) -> Instant {
        self.fetched_at
    }

    /// Whether the list may still be reused at `now`.
    pub fn is_fresh_at(&self, now: Instant) -> bool {
        now.saturating_duration_since(self.fetched_at) < self.ttl
    }

    pub fn is_fresh(&self) -> bool {
        self.is_fresh_at(Instant::now())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tool(name: &'static str) -> Tool {
        Tool::new(name, "test", Arc::new(serde_json::Map::new()))
    }

    #[test]
    fn server_hint_sets_ttl() {
        let t0 = Instant::now();
        let c = Catalog::new(
            vec![tool("amp_whoami"), tool("amp_get_task")],
            t0,
            Some(300_000),
            Some(CacheScope::Private),
            Duration::ZERO,
        );
        assert_eq!(c.ttl(), Duration::from_secs(300));
        assert_eq!(c.ttl_source(), TtlSource::Server);
        assert_eq!(c.cache_scope(), Some(CacheScope::Private));
        assert!(c.is_fresh_at(t0 + Duration::from_secs(299)));
        assert!(!c.is_fresh_at(t0 + Duration::from_secs(300)));
        assert_eq!(
            c.names().collect::<Vec<_>>(),
            ["amp_whoami", "amp_get_task"]
        );
        assert!(c.get("amp_get_task").is_some());
        assert!(c.get("amp_amp_get_task").is_none());
    }

    #[test]
    fn no_hint_uses_default_and_zero_means_always_stale() {
        let t0 = Instant::now();
        let c = Catalog::new(vec![], t0, None, None, Duration::ZERO);
        assert_eq!(c.ttl_source(), TtlSource::Default);
        assert!(!c.is_fresh_at(t0));
        let c = Catalog::new(vec![], t0, None, None, Duration::from_secs(60));
        assert!(c.is_fresh_at(t0 + Duration::from_secs(59)));
    }

    #[test]
    fn ttl_is_capped() {
        let c = Catalog::new(vec![], Instant::now(), Some(u64::MAX), None, Duration::ZERO);
        assert_eq!(c.ttl(), MAX_CATALOG_TTL);
    }
}
