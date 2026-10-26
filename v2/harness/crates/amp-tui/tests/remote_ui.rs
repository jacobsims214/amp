//! #2376: `/remote` in a real chat `Shell`, against #2373's in-process mock relay. The test plays
//! the browser and reads the screen: the badge, the pairing link with (or without) its QR code,
//! the local-only y/N with No as the default, a remote prompt tagged `[remote: …]`, `/remote
//! devices` (list, revoke), the notices on a relay-side end, and the errors before turning on.
//!
//! #2423: the link is copied as it is shown, `/remote copy` and `/remote open` work, and a plain
//! selection of its rows is the link. The clipboard and browser are recording stand-ins on the
//! chat's search path (`pbcopy`, `open`) plus a recording OSC 52 writer: nothing here touches
//! the real clipboard or browser.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{
    agent::memory_session,
    events::{ChannelEmitter, FanOutEmitter},
    manager_feed::ManagerFeed,
    remote::{
        FixedToken, LinkTuning, RemoteDeps, RemoteLink, RemoteSetup,
        crypto::{self, KeyPair, KeyStore},
        envelope,
        mock_relay::{MockOptions, MockRelay},
        pairing,
        redact::Redactor,
        relay::now_ms,
    },
};
use amp_tui::{Desktop, Platform, RunDone, Shell, ShellInfo, clipboard::osc52};
use rpi_agent::AgentEvent;
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use serde_json::{Value, json};
use tokio::sync::mpsc::UnboundedReceiver;

const WAIT: Duration = Duration::from_secs(20);
const USER: i64 = 1001;

struct Rig {
    shell: Shell,
    events: UnboundedReceiver<AgentEvent>,
    done: UnboundedReceiver<RunDone>,
    mock: MockRelay,
    script: FauxScript,
    dir: std::path::PathBuf,
    /// The stand-in clipboard and browser tools (#2423).
    stubs: PathBuf,
    /// What the chat sent the terminal (OSC 52).
    terminal: Arc<Mutex<Vec<String>>>,
}

/// A recording stand-in for a tool: `pbcopy` keeps its input, `open` its argument, each run in
/// its own `<name>.<pid>.out`.
fn stub(dir: &Path, name: &str, body: &str) {
    use std::os::unix::fs::PermissionsExt;
    let path = dir.join(name);
    std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
}

/// What each run of the stand-in `name` recorded.
fn recorded(dir: &Path, name: &str) -> Vec<String> {
    let mut out: Vec<(std::time::SystemTime, String)> = std::fs::read_dir(dir)
        .unwrap()
        .filter_map(Result::ok)
        .filter(|e| {
            let n = e.file_name().to_string_lossy().into_owned();
            n.starts_with(&format!("{name}.")) && n.ends_with(".out")
        })
        .map(|e| {
            (
                e.metadata().unwrap().modified().unwrap(),
                std::fs::read_to_string(e.path()).unwrap(),
            )
        })
        .collect();
    out.sort();
    out.into_iter().map(|(_, s)| s).collect()
}

impl Drop for Rig {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn tuning() -> LinkTuning {
    LinkTuning {
        batch_every: Duration::from_millis(30),
        silence: Duration::from_secs(5),
        backoff_base: Duration::from_millis(100),
        backoff_max: Duration::from_millis(400),
        status_min: Duration::from_millis(200),
        status_max: Duration::from_secs(60),
        tick: Duration::from_millis(50),
        bye_wait: Duration::from_secs(3),
        refused_every: Duration::ZERO,
        end_retry: Duration::from_millis(200),
    }
}

async fn rig(tag: &str, opts: MockOptions, size: (usize, usize)) -> Rig {
    let script = FauxScript::new();
    let provider = FauxProvider::new(script.clone());
    let model = provider.default_model().clone();
    let (tui, events) = ChannelEmitter::new();
    let fanout = FanOutEmitter::new();
    fanout.add_sink(tui.into_emitter());
    let feed = ManagerFeed::new();
    fanout.add_sink(feed.emitter());
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(fanout.clone().into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        requested_thinking: None,
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: Some("remote-m1-2376".into()),
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, done) = Shell::new(Arc::new(harness), info, vec![]);
    shell.set_feed(Some(Arc::clone(&feed)));
    shell.set_screen(size.0, size.1);
    let mock = MockRelay::start(opts.clone()).await;
    let dir = std::env::temp_dir().join(format!(
        "amp-remote-ui-{tag}-{}-{}",
        std::process::id(),
        crypto::b64u(&crypto::random_bytes(6))
    ));
    std::fs::create_dir_all(&dir).unwrap();
    let stubs = dir.join("bin");
    std::fs::create_dir_all(&stubs).unwrap();
    stub(&stubs, "pbcopy", "cat > \"$0.$$.out\"");
    stub(&stubs, "open", "printf '%s' \"$1\" > \"$0.$$.out\"");
    let terminal = Arc::new(Mutex::new(Vec::new()));
    let sink = Arc::clone(&terminal);
    // macOS's tools on every OS, so CI on Linux runs the same stand-ins.
    shell.set_desktop(
        Desktop::with_path(Platform::Mac, &stubs).with_terminal(Arc::new(move |s: &str| {
            sink.lock().unwrap().push(s.to_owned())
        })),
    );
    let link = RemoteLink::new(RemoteDeps {
        setup: RemoteSetup {
            api_base: mock.base.clone(),
            web_origin: "https://amp.local".into(),
            ca_file: None,
            project_id: Some(2),
            host: "test-host".into(),
            os: "macos".into(),
            version: "0.0.0-test".into(),
        },
        tokens: Arc::new(FixedToken(opts.oauth_token.clone(), USER)),
        host: shell.remote_host(),
        feed,
        keys: KeyStore::memory(),
        pins: Some(dir.join("remote-devices.json")),
        action_log: Some(dir.join("remote-actions.log")),
        pending_ends: None,
        redactor: Redactor::new(),
        switches: Arc::new(|| Ok(())),
        tuning: tuning(),
    });
    shell.set_remote(Ok(link), None);
    Rig {
        shell,
        events,
        done,
        mock,
        script,
        dir,
        stubs,
        terminal,
    }
}

impl Rig {
    /// Drives the shell (as the terminal loop does) until the screen holds `needle`.
    async fn until(&mut self, needle: &str) -> String {
        let deadline = tokio::time::Instant::now() + WAIT;
        loop {
            self.shell.poll();
            let screen = self.screen();
            if screen.contains(needle) {
                return screen;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "no `{needle}` on screen:\n{screen}"
            );
            tokio::select! {
                Some(e) = self.events.recv() => self.shell.handle_event(&e),
                Some(r) = self.done.recv() => {
                    while let Ok(e) = self.events.try_recv() {
                        self.shell.handle_event(&e);
                    }
                    self.shell.finish_run(r);
                }
                _ = tokio::time::sleep(Duration::from_millis(20)) => {}
            }
        }
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(200, 300).join("\n")
    }

    fn hello(&self) -> Value {
        self.mock
            .events()
            .into_iter()
            .find(|e| e["op"] == "hello")
            .expect("hello")
    }

    /// The pairing link on screen.
    fn link(&self) -> String {
        self.screen()
            .lines()
            .rev()
            .map(str::trim)
            .find(|l| l.starts_with("https://amp.local/sessions/"))
            .expect("a pairing link")
            .to_owned()
    }

    /// Sends a pair request for `device` with the link on screen; returns the code.
    fn pair_request(&self, device: &KeyPair) -> String {
        let (secret, h) = pairing::parse_link_fragment(&self.link()).unwrap();
        let hello = self.hello();
        let sid = self.mock.session_id().unwrap();
        assert_eq!(hello["harness_id"], h.as_str());
        let msg =
            pairing::build_pair_request(&secret, &sid, &h, device, "Safari on iPhone", now_ms());
        assert!(self.mock.send_pair_request(msg, USER, "jsims@example.test"));
        let harness_pub =
            crypto::b64u_decode(hello["body"]["harness"]["harness_pub"].as_str().unwrap()).unwrap();
        pairing::sas_code(&secret, &sid, &harness_pub, device.public_raw())
    }

    async fn wait_hello(&mut self) {
        let deadline = tokio::time::Instant::now() + WAIT;
        while !self.mock.events().iter().any(|e| e["op"] == "hello") {
            assert!(tokio::time::Instant::now() < deadline, "no hello");
            self.shell.poll();
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    async fn pair_result(&mut self, n: usize) -> Value {
        let deadline = tokio::time::Instant::now() + WAIT;
        loop {
            self.shell.poll();
            let results: Vec<Value> = self
                .mock
                .events()
                .into_iter()
                .filter(|e| e["op"] == "pair_result")
                .collect();
            if results.len() >= n {
                return results[n - 1].clone();
            }
            assert!(tokio::time::Instant::now() < deadline, "no pair_result {n}");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_in_the_chat() {
    let mut r = rig("flow", MockOptions::default(), (120, 40)).await;
    r.script.set_responses(vec![FauxStep::text(
        "Hi phone, this reply came from the laptop.",
    )]);

    // Before it is on.
    assert!(r.shell.submit("/remote devices").await == amp_tui::Submit::Continue);
    r.until("No paired devices").await;
    r.shell.submit("/remote pair").await;
    r.until("remote control is off; /remote turns it on first")
        .await;

    // On: the badge, the link, the QR code (120x40 has room) and when it ends.
    r.shell.submit("/remote").await;
    let screen = r.until("REMOTE ON · 0 devices").await;
    let screen = if screen.contains("https://amp.local/sessions/") {
        screen
    } else {
        r.until("https://amp.local/sessions/").await
    };
    // #2424: no limit by default, so no end time.
    assert!(
        screen.contains("Remote control is on until you end it: it reconnects by itself"),
        "{screen}"
    );
    assert!(screen.contains("It works once, for 5 min"), "{screen}");
    assert!(screen.contains("████"), "the QR code:\n{screen}");
    assert!(!screen.contains("No QR code"), "{screen}");

    // A pairing question: Enter is No.
    let device = KeyPair::generate();
    r.wait_hello().await;
    let code = r.pair_request(&device);
    let screen = r
        .until("Allow \"Safari on iPhone\" (jsims@example.test) to control this session?")
        .await;
    assert!(screen.contains(&format!("code {code} [y/N]")), "{screen}");
    assert!(screen.contains("Only this keyboard can answer"), "{screen}");
    assert!(r.shell.pairing_pending());
    assert!(!r.shell.input_focused(), "the question has the keys");
    // A remote prompt can't answer it: it is only a prompt (and this device isn't paired).
    r.shell.press("enter").await.unwrap();
    assert_eq!(r.pair_result(1).await["body"]["result"], "rejected");
    r.until("Refused \"Safari on iPhone\": it was not paired.")
        .await;

    // A new link; y pairs it.
    r.shell.submit("/remote pair").await;
    let deadline = tokio::time::Instant::now() + WAIT;
    while r.screen().matches("https://amp.local/sessions/").count() < 2 {
        assert!(tokio::time::Instant::now() < deadline, "no second link");
        r.shell.poll();
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let code = r.pair_request(&device);
    r.until(&format!("code {code} [y/N]")).await;
    r.shell.press("y").await.unwrap();
    let res = r.pair_result(2).await;
    assert_eq!(res["body"]["result"], "accepted", "{res}\n{}", r.screen());
    r.until("Allowed \"Safari on iPhone\"").await;
    assert!(r.mock.set_viewers(1));
    r.until("REMOTE ON · 1 device").await;

    // A remote prompt shows tagged.
    let hid = r.hello()["harness_id"].as_str().unwrap().to_owned();
    let cmd = envelope::sign_command(
        &device,
        &r.mock.session_id().unwrap(),
        &hid,
        now_ms(),
        now_ms(),
        &crypto::new_nonce(),
        "prompt",
        json!({"text": "hello from the phone", "client_msg_id": "m-1"}),
    );
    assert!(r.mock.send_command(&cmd));
    let screen = r.until("this reply came from the laptop").await;
    let tag = screen
        .lines()
        .position(|l| l.trim() == "[remote: Safari on iPhone]")
        .unwrap_or_else(|| panic!("the tag line:\n{screen}"));
    assert!(
        screen
            .lines()
            .nth(tag + 1)
            .unwrap()
            .contains("hello from the phone"),
        "{screen}"
    );

    // Devices: listed, then revoked.
    r.shell.submit("/remote devices").await;
    r.until("1. Safari on iPhone · paired").await;
    r.shell.submit("/remote devices revoke 2").await;
    r.until("there is no device 2").await;
    r.shell.submit("/remote devices revoke 1").await;
    r.until("Revoked \"Safari on iPhone\"").await;

    // The relay ends it (Disconnect on the web page): a notice, and the badge goes.
    r.mock.end_session("disconnected");
    let screen = r
        .until("Remote control turned off from the web app (Disconnect).")
        .await;
    assert!(!screen.contains("REMOTE ON"), "{screen}");
    r.shell.submit("/remote off").await;
    r.until("Remote control is already off.").await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_80x24_window_gets_the_link_alone_and_off_says_so() {
    let mut r = rig("small", MockOptions::default(), (80, 24)).await;
    r.shell.submit("/remote").await;
    let screen = r.until("No QR code").await;
    assert!(screen.contains("https://amp.local/sessions/"), "{screen}");
    assert!(!screen.contains("████"), "{screen}");
    r.shell.submit("/remote off").await;
    let screen = r.until("Remote control is off.").await;
    assert!(r.mock.ended().is_some());
    assert!(!screen.contains("REMOTE ON"), "{screen}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn refusals_are_clear() {
    // An AMP without remote control (404) and an org with it off (403).
    let mut r = rig(
        "404",
        MockOptions {
            refuse_register: Some((404, "not_found")),
            ..MockOptions::default()
        },
        (80, 24),
    )
    .await;
    r.shell.submit("/remote").await;
    r.until("/remote: Remote control isn't available on this AMP server.")
        .await;
    let mut r = rig(
        "403",
        MockOptions {
            refuse_register: Some((403, "remote_disabled")),
            ..MockOptions::default()
        },
        (80, 24),
    )
    .await;
    r.shell.submit("/remote").await;
    r.until("/remote: Remote control is turned off for this project's organization")
        .await;
    // Why it can't be used from here at all (the login, a switch): said as is.
    r.shell.set_remote(
        Err("remote control needs your AMP login; run `amp-harness login`".into()),
        None,
    );
    r.shell.submit("/remote").await;
    r.until("/remote: remote control needs your AMP login")
        .await;
}

/// #2424: `/remote` and `/remote status` say when remote control ends: never by itself by
/// default (the footer is a plain `REMOTE ON`), or at the organization's limits when it set them.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_says_it_has_no_end_time_unless_the_org_set_one() {
    let mut r = rig("nolimit", MockOptions::default(), (120, 40)).await;
    r.shell.submit("/remote").await;
    r.until("REMOTE ON · 0 devices").await;
    r.wait_hello().await;
    r.shell.submit("/remote status").await;
    let screen = r
        .until("It stays on, through sleep and network changes")
        .await;
    assert!(!screen.contains("turns off at"), "{screen}");
    assert!(!screen.contains("without remote activity"), "{screen}");
    r.shell.submit("/remote off").await;
    r.until("Remote control is off.").await;

    let opts = MockOptions {
        idle_timeout_s: Some(1800),
        lifetime_ms: Some(12 * 3_600_000),
        ..MockOptions::default()
    };
    let mut r = rig("limits", opts, (120, 40)).await;
    r.shell.submit("/remote").await;
    let screen = r.until("REMOTE ON · 0 devices").await;
    let screen = if screen.contains("(your organization's limit)") {
        screen
    } else {
        r.until("(your organization's limit)").await
    };
    assert!(
        screen.contains("(12 h), or after 30 min without remote activity"),
        "{screen}"
    );
    r.wait_hello().await;
    r.shell.submit("/remote status").await;
    r.until("without remote activity (now at").await;
    r.shell.submit("/remote off").await;
    r.until("Remote control is off.").await;
}

/// #2423: the pairing link is copied as it is shown (the stand-in `pbcopy` and OSC 52 get it,
/// exactly), `/remote copy` copies it again, `/remote open` opens it, and the rows it is drawn
/// on, selected and joined, are the link: no padding, no leading space.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_pairing_link_is_copied_opened_and_selectable() {
    let mut r = rig("copy", MockOptions::default(), (80, 24)).await;

    r.shell.submit("/remote copy").await;
    r.until("/remote copy: remote control is off").await;
    r.shell.submit("/remote open").await;
    r.until("/remote open: remote control is off").await;
    assert!(recorded(&r.stubs, "pbcopy").is_empty());

    r.shell.submit("/remote").await;
    let screen = r.until("Pairing link copied to the clipboard.").await;
    assert!(
        screen.contains("/remote copy copies the link again"),
        "{screen}"
    );
    assert!(screen.contains("Option-drag (iTerm2)"), "{screen}");
    let copies = recorded(&r.stubs, "pbcopy");
    assert_eq!(copies.len(), 1, "{copies:?}");
    let link = copies[0].clone();
    assert!(link.starts_with("https://amp.local/sessions/"), "{link}");
    pairing::parse_link_fragment(&link).expect("the copied link is a pairing link");
    assert_eq!(*r.terminal.lock().unwrap(), vec![osc52(&link)]);
    // The link itself is never in a notice (only in its own block).
    assert_eq!(screen.matches(&link[..40]).count(), 1, "{screen}");

    // A plain selection: from the first character of the link to the last, at a width that
    // wraps it. The rows start at the left edge, so joined they are the link.
    for width in [60, 80, 120] {
        let rows: Vec<String> = r
            .shell
            .view()
            .render_frame(width, 300)
            .iter()
            .map(|l| rpi_tui::strip_ansi(l).trim_end().to_owned())
            .collect();
        let first = rows
            .iter()
            .position(|l| l.starts_with("https://amp.local/sessions/"))
            .unwrap_or_else(|| panic!("the link at column 0 ({width}):\n{}", rows.join("\n")));
        let selected: Vec<&String> = rows[first..]
            .iter()
            .take_while(|l| !l.is_empty() && !l.starts_with(' ') && !l.contains(' '))
            .collect();
        if width < 100 {
            assert!(selected.len() > 1, "it wraps at {width}");
        }
        let joined: String = selected.iter().map(|s| s.as_str()).collect();
        assert_eq!(joined, link, "a selection at {width} columns");
    }

    // Again, and open.
    r.shell.submit("/remote copy").await;
    let deadline = tokio::time::Instant::now() + WAIT;
    while recorded(&r.stubs, "pbcopy").len() < 2 {
        assert!(tokio::time::Instant::now() < deadline, "no second copy");
        r.shell.poll();
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(
        recorded(&r.stubs, "pbcopy"),
        vec![link.clone(), link.clone()]
    );
    assert_eq!(r.terminal.lock().unwrap().len(), 2);
    r.shell.submit("/remote open").await;
    r.until("Opened the pairing link in this computer's browser")
        .await;
    assert_eq!(recorded(&r.stubs, "open"), vec![link.clone()]);

    // A failing pbcopy: OSC 52 still went, and the chat says what failed (not the link).
    stub(&r.stubs, "pbcopy", "exit 3");
    r.shell.submit("/remote copy").await;
    let screen = r.until("(OSC 52) (pbcopy: exited with").await;
    assert!(screen.contains("If pasting gives nothing"), "{screen}");
    assert_eq!(r.terminal.lock().unwrap().len(), 3);

    // No clipboard and no browser at all.
    r.shell.set_desktop(Desktop::off());
    r.shell.submit("/remote copy").await;
    r.until("/remote copy: no clipboard here; /remote open, or select the link.")
        .await;
    r.shell.submit("/remote open").await;
    r.until("/remote open: couldn't open the browser (no browser in this session)")
        .await;

    // Off: nothing to copy.
    r.shell.submit("/remote off").await;
    r.until("Remote control is off.").await;
    r.shell.submit("/remote copy").await;
    let deadline = tokio::time::Instant::now() + WAIT;
    while r
        .screen()
        .matches("/remote copy: remote control is off")
        .count()
        < 2
    {
        assert!(tokio::time::Instant::now() < deadline, "no second refusal");
        r.shell.poll();
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// #2423: with mouse capture off the hint says plain selection works.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_mouse_capture_the_hint_says_select_it() {
    let mut r = rig("nomouse", MockOptions::default(), (80, 24)).await;
    r.shell.set_mouse_capture(false);
    r.shell.submit("/remote").await;
    let screen = r.until("Pairing link copied to the clipboard.").await;
    assert!(screen.contains("or select it with the mouse"), "{screen}");
    assert!(!screen.contains("Option-drag"), "{screen}");
}
