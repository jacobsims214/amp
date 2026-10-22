//! #2159: the shared client's connection is replaceable. After a "login required" it reconnects
//! through its [`Reconnector`] (rate-limited, single flight) and retries on the new connection;
//! calls cut off by a swap fail at once; without a reconnector (AMP_TOKEN) nothing changes.
//!
//! Two servers play the two logins: `old` stands for the connection made before
//! `amp-harness login` (it answers 401 once its tokens are dead), `new` for the one a fresh
//! connect makes afterwards.

mod common;

use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use amp_mcp::{
    AmpMcpClient, ClientOptions, DEFAULT_RECONNECT_INTERVAL, Health, McpError, Reconnector,
};
use common::{ServerKind, Switchable, TestServer, spawn_switchable};

fn options(url: &str) -> ClientOptions {
    ClientOptions::new(url).with_connect_timeout(Duration::from_secs(5))
}

async fn connect(url: &str) -> AmpMcpClient {
    AmpMcpClient::connect(options(url)).await.expect("connect")
}

async fn pair() -> (Switchable, Switchable) {
    (
        spawn_switchable(TestServer::new(ServerKind::Dual)).await,
        spawn_switchable(TestServer::new(ServerKind::Dual)).await,
    )
}

/// Connects to `new` once `logged_in` is set, else to `old`; counts attempts.
fn reconnector(
    old: &str,
    new: &str,
    logged_in: &Arc<AtomicBool>,
    attempts: &Arc<AtomicUsize>,
) -> Reconnector {
    let (old, new) = (old.to_owned(), new.to_owned());
    let (logged_in, attempts) = (logged_in.clone(), attempts.clone());
    Arc::new(move || {
        attempts.fetch_add(1, Ordering::SeqCst);
        let url = if logged_in.load(Ordering::SeqCst) {
            new.clone()
        } else {
            old.clone()
        };
        Box::pin(async move { AmpMcpClient::connect(options(&url)).await })
    })
}

#[test]
fn reconnects_at_most_every_15_s_by_default() {
    assert_eq!(DEFAULT_RECONNECT_INTERVAL, Duration::from_secs(15));
}

#[tokio::test(flavor = "multi_thread")]
async fn login_required_heals_through_the_reconnector_within_the_interval() {
    let (old, new) = pair().await;
    let client = Arc::new(connect(&old.running.url).await);
    let (logged_in, attempts) = (
        Arc::new(AtomicBool::new(false)),
        Arc::new(AtomicUsize::new(0)),
    );
    let interval = Duration::from_millis(400);
    client.set_reconnector(Some(reconnector(
        &old.running.url,
        &new.running.url,
        &logged_in,
        &attempts,
    )));
    client.set_reconnect_interval(interval);
    assert!(client.can_reconnect());
    client
        .call_tool("amp_whoami", None)
        .await
        .expect("first call");
    assert_eq!(
        attempts.load(Ordering::SeqCst),
        0,
        "a working client never reconnects"
    );

    // The login's tokens are dead: the call fails, one reconnect attempt is made (to the old
    // login, which fails too) and the error is the usual "login required".
    old.unauthorized.store(true, Ordering::SeqCst);
    let err = client.call_tool("amp_whoami", None).await.unwrap_err();
    assert!(err.is_auth_required(), "{err:?}");
    assert_eq!(attempts.load(Ordering::SeqCst), 1);
    assert_eq!(client.health(), Health::AuthRequired);

    // Inside the interval nothing is retried: a model calling in a loop costs one request each.
    let requests = old.requests.load(Ordering::SeqCst);
    for _ in 0..5 {
        let err = client.call_tool("amp_whoami", None).await.unwrap_err();
        assert!(err.is_auth_required(), "{err:?}");
    }
    assert_eq!(attempts.load(Ordering::SeqCst), 1, "rate-limited");
    assert_eq!(old.requests.load(Ordering::SeqCst) - requests, 5);

    // `amp-harness login` in another terminal. The first call after the interval reconnects and
    // runs on the new connection; the caller just sees its result.
    logged_in.store(true, Ordering::SeqCst);
    tokio::time::sleep(interval).await;
    let started = Instant::now();
    let result = client
        .call_tool(
            "amp_echo",
            Some(serde_json::Map::from_iter([("n".into(), 1.into())])),
        )
        .await
        .expect("healed");
    assert!(started.elapsed() < Duration::from_secs(5));
    assert_eq!(result.is_error, Some(false));
    assert_eq!(attempts.load(Ordering::SeqCst), 2);
    assert_eq!(client.generation(), 1);
    assert_eq!(client.health(), Health::Connected);
    assert_eq!(client.session().url, new.running.url);
    assert_eq!(new.running.counters.call_tool.load(Ordering::SeqCst), 1);

    // From now on everything goes to the new connection, the catalog included, and the old
    // server is never called again even once it would answer.
    old.unauthorized.store(false, Ordering::SeqCst);
    let old_requests = old.requests.load(Ordering::SeqCst);
    client.call_tool("amp_whoami", None).await.expect("again");
    let catalog = client.catalog().await.expect("catalog");
    assert!(catalog.get("amp_whoami").is_some());
    assert_eq!(new.running.counters.list_tools(), 1);
    assert_eq!(old.requests.load(Ordering::SeqCst), old_requests);
    assert_eq!(attempts.load(Ordering::SeqCst), 2);
}

#[tokio::test(flavor = "multi_thread")]
async fn concurrent_failures_share_one_reconnect() {
    let (old, new) = pair().await;
    let client = Arc::new(connect(&old.running.url).await);
    let (logged_in, attempts) = (
        Arc::new(AtomicBool::new(true)),
        Arc::new(AtomicUsize::new(0)),
    );
    client.set_reconnector(Some(reconnector(
        &old.running.url,
        &new.running.url,
        &logged_in,
        &attempts,
    )));
    old.unauthorized.store(true, Ordering::SeqCst);

    let calls: Vec<_> = (0..8)
        .map(|_| {
            let client = client.clone();
            tokio::spawn(async move { client.call_tool("amp_whoami", None).await })
        })
        .collect();
    for call in calls {
        call.await.unwrap().expect("every call heals");
    }
    assert_eq!(attempts.load(Ordering::SeqCst), 1, "single flight");
    assert_eq!(client.generation(), 1);
    assert_eq!(new.running.counters.call_tool.load(Ordering::SeqCst), 8);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_call_in_flight_during_a_swap_fails_at_once() {
    let (old, new) = pair().await;
    let client = Arc::new(connect(&old.running.url).await);
    let slow = {
        let client = client.clone();
        tokio::spawn(async move {
            let started = Instant::now();
            let args = serde_json::Map::from_iter([("ms".into(), 20_000.into())]);
            let result = client.call_tool("amp_sleep", Some(args)).await;
            (result, started.elapsed())
        })
    };
    // Let the request reach the server before swapping.
    let deadline = Instant::now() + Duration::from_secs(5);
    while old.running.counters.call_tool.load(Ordering::SeqCst) == 0 {
        assert!(Instant::now() < deadline, "the slow call never arrived");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    client.replace(connect(&new.running.url).await);
    let (result, took) = tokio::time::timeout(Duration::from_secs(5), slow)
        .await
        .expect("the in-flight call must not hang")
        .unwrap();
    let err = result.unwrap_err();
    assert!(matches!(err, McpError::Replaced { .. }), "{err:?}");
    assert!(err.to_string().contains("may or may not have run"), "{err}");
    assert!(took < Duration::from_secs(5), "{took:?}");
    // The cut-off call neither changed the health nor was retried on the new connection.
    assert_eq!(client.health(), Health::Connected);
    assert_eq!(new.running.counters.call_tool.load(Ordering::SeqCst), 0);
    client
        .call_tool("amp_whoami", None)
        .await
        .expect("new connection");
    assert_eq!(new.running.counters.call_tool.load(Ordering::SeqCst), 1);
}

/// #2164's `/server`: point the reconnector at another server and reconnect now (no rate limit).
#[tokio::test(flavor = "multi_thread")]
async fn reconnect_now_switches_to_another_server() {
    let (a, b) = pair().await;
    let client = Arc::new(connect(&a.running.url).await);
    client.catalog().await.expect("catalog on a");
    let err = client.reconnect().await.unwrap_err();
    assert!(err.to_string().contains("no reconnector"), "{err}");

    let to_b: Reconnector = {
        let url = b.running.url.clone();
        Arc::new(move || {
            let url = url.clone();
            Box::pin(async move { AmpMcpClient::connect(options(&url)).await })
        })
    };
    client.set_reconnector(Some(to_b));
    for round in 1..=2 {
        client.reconnect().await.expect("switch");
        assert_eq!(client.generation(), round);
    }
    assert_eq!(client.session().url, b.running.url);
    client.catalog().await.expect("catalog on b");
    assert_eq!(
        b.running.counters.list_tools(),
        1,
        "the catalog belongs to the connection"
    );
    client.call_tool("amp_whoami", None).await.expect("on b");
    assert_eq!(a.running.counters.call_tool.load(Ordering::SeqCst), 0);

    // A failed reconnect keeps the current connection.
    let broken: Reconnector = Arc::new(|| {
        Box::pin(async { AmpMcpClient::connect(options("http://127.0.0.1:9/mcp")).await })
    });
    client.set_reconnector(Some(broken));
    assert!(client.reconnect().await.is_err());
    assert_eq!(client.session().url, b.running.url);
    client
        .call_tool("amp_whoami", None)
        .await
        .expect("still on b");
}

/// AMP_TOKEN: no reconnector, so a rejected token stays rejected and nothing reconnects.
#[tokio::test(flavor = "multi_thread")]
async fn without_a_reconnector_login_required_stays() {
    let (old, _new) = pair().await;
    let client = Arc::new(connect(&old.running.url).await);
    assert!(!client.can_reconnect());
    old.unauthorized.store(true, Ordering::SeqCst);
    for _ in 0..3 {
        let err = client.call_tool("amp_whoami", None).await.unwrap_err();
        assert!(err.is_auth_required(), "{err:?}");
    }
    assert_eq!(client.generation(), 0);
    assert_eq!(client.health(), Health::AuthRequired);
}
