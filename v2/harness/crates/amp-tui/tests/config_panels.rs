//! #2245: the configuration panels' mechanics in the shell, with a fake [`ConfigControl`] (the
//! real one, amp-harness's `chat_config`, is tested end to end in amp-harness/tests/chat_panels.rs):
//! - `/mcp`, `/agents`, `/skills`, `/config` and `/model <role> …` go to the control; `/model
//!   provider/model` still switches this chat's model;
//! - Enter and the letter keys run the panel's slash commands for the selected row, and only the
//!   keys that apply to that row (the hint says which);
//! - a reply that changed files runs the `/reload` path once, saying "applied";
//! - errors go to the transcript and the panel's note;
//! - a panel longer than it shows scrolls past its last selectable row;
//! - the AMP status line shows `mcp 3/4`.
//!
//! Real rpi `AgentHarness` on the faux provider; no terminal.

use std::{
    sync::{Arc, Mutex},
    time::Duration,
};

use amp_core::{
    agent::memory_session,
    events::ChannelEmitter,
    mcp_hub::{HubLevel, HubSummary},
};
use amp_tui::{
    Cell, ConfigControl, ConfigFuture, ConfigReply, InfoPanel, InfoRow, ReloadControl,
    ReloadFuture, ReloadReply, Shell, ShellInfo, Tone, chrome,
};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};

const W: usize = 100;
const H: usize = 40;

type Calls = Arc<Mutex<Vec<(String, String)>>>;

/// Records every command and answers like the real control would, from canned panels.
struct FakeConfig {
    calls: Calls,
}

fn servers_panel() -> InfoPanel {
    let rows = vec![
        InfoRow::new(vec![Cell::new("2 of 3 enabled servers ready", Tone::Dim)]),
        InfoRow::new(vec![
            Cell::new("●", Tone::Success),
            Cell::new("fs", Tone::Text).pad(6),
            Cell::flex("ready · 2 tools", Tone::Success),
        ])
        .key("fs")
        .keys(&['r', 't', 'd']),
        InfoRow::new(vec![
            Cell::new("○", Tone::Dim),
            Cell::new("off", Tone::Text).pad(6),
            Cell::flex("disabled", Tone::Dim),
        ])
        .key("off")
        .keys(&['e']),
        InfoRow::text("add one: /mcp add <name> <url>", Tone::Dim, 0),
    ];
    InfoPanel::fixed("MCP servers", rows)
        .on_enter("details", "/mcp {}")
        .with_key('r', "restart", "/mcp restart {}")
        .with_key('t', "test", "/mcp test {}")
        .with_key('e', "enable", "/mcp enable {}")
        .with_key('d', "disable", "/mcp disable {}")
}

impl ConfigControl for FakeConfig {
    fn run(&self, name: &str, args: &str) -> ConfigFuture {
        self.calls
            .lock()
            .unwrap()
            .push((name.to_owned(), args.to_owned()));
        let reply: Result<ConfigReply, String> = match (name, args) {
            ("mcp", "") => Ok(ConfigReply::panel(servers_panel())),
            ("mcp", "enable off") => {
                let mut r = ConfigReply::done("MCP server `off` enabled (config.toml)");
                r.reload = true;
                Ok(r)
            }
            ("mcp", "restart fs") => Ok(ConfigReply::done("MCP server `fs`: restarting")),
            ("mcp", "nope") => Err("no MCP server `nope` (/mcp lists them)".to_owned()),
            ("config", "") => {
                let rows = (0..40)
                    .map(|i| {
                        InfoRow::field(&format!("key {i}"), 8, format!("value {i}"), Tone::Text)
                    })
                    .collect();
                Ok(ConfigReply::panel(InfoPanel::fixed("config", rows)))
            }
            _ => Ok(ConfigReply::done(format!("ran /{name} {args}"))),
        };
        Box::pin(async move { reply })
    }
}

/// Counts reloads; reports one changed line.
struct FakeReload(Arc<Mutex<usize>>);

impl ReloadControl for FakeReload {
    fn reload(&self) -> ReloadFuture {
        *self.0.lock().unwrap() += 1;
        Box::pin(async {
            Ok(ReloadReply {
                lines: vec!["mcp: +off".to_owned()],
                providers: Vec::new(),
                models: Vec::new(),
                switch_to: None,
                stays: None,
                warnings: Vec::new(),
            })
        })
    }
}

struct Rig {
    shell: Shell,
    calls: Calls,
    reloads: Arc<Mutex<usize>>,
}

async fn rig(with_config: bool) -> Rig {
    let script = FauxScript::new();
    script.set_responses(vec![FauxStep::text("ok")]);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, _events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
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
        git_branch: None,
        session_file: None,
        banner: vec!["AMP harness · manager".into()],
    };
    let (mut shell, _done) = Shell::new(Arc::new(harness), info, Vec::new());
    let calls: Calls = Arc::default();
    let reloads = Arc::new(Mutex::new(0));
    if with_config {
        shell.set_config(Arc::new(FakeConfig {
            calls: calls.clone(),
        }));
        shell.set_reload(Arc::new(FakeReload(reloads.clone())));
    }
    Rig {
        shell,
        calls,
        reloads,
    }
}

impl Rig {
    async fn submit(&mut self, text: &str) {
        self.shell.submit(text).await;
        self.settle().await;
    }

    async fn settle(&mut self) {
        self.shell
            .settle(Duration::from_secs(5))
            .await
            .expect("settled");
    }

    async fn key(&mut self, c: char) -> bool {
        let used = self
            .shell
            .key(&KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE), true)
            .await;
        self.settle().await;
        used
    }

    async fn code(&mut self, code: KeyCode) {
        self.shell
            .key(&KeyEvent::new(code, KeyModifiers::NONE), true)
            .await;
        self.settle().await;
    }

    fn screen(&self) -> String {
        self.shell.view().render_plain(W, H).join("\n")
    }

    fn calls(&self) -> Vec<(String, String)> {
        self.calls.lock().unwrap().clone()
    }

    fn panel_lines(&self) -> Vec<String> {
        let screen = self.shell.view().render_plain(W, H);
        let Some(start) = screen
            .iter()
            .position(|l| l.trim_start().starts_with("── "))
        else {
            return Vec::new();
        };
        screen[start..]
            .iter()
            .take_while(|l| !l.contains("/ for commands"))
            .map(|l| l.trim_end().to_owned())
            .collect()
    }
}

fn flat(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn keys_run_the_selected_rows_commands_and_only_its_own() {
    let mut rig = rig(true).await;
    rig.submit("/mcp").await;
    assert_eq!(rig.calls(), vec![("mcp".to_owned(), String::new())]);
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("── MCP servers"), "{panel}");
    assert!(panel.contains("› ● fs     ready · 2 tools"), "{panel}");
    // The hint names Enter's word and the selected row's keys only.
    assert!(
        panel.contains("↑↓ select · Enter details · r restart · t test · d disable · Esc close"),
        "{panel}"
    );
    assert!(!panel.contains("e enable"), "{panel}");

    // `e` does not apply to `fs`: not used, nothing run.
    assert!(!rig.key('e').await);
    assert_eq!(rig.calls().len(), 1);
    // `r` runs `/mcp restart fs`; the panel stays, with the note.
    assert!(rig.key('r').await);
    assert_eq!(rig.calls()[1], ("mcp".to_owned(), "restart fs".to_owned()));
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("MCP server `fs`: restarting"), "{panel}");
    assert!(rig.screen().contains("MCP server `fs`: restarting"));

    // ↓ to `off`: now only `e` applies.
    rig.code(KeyCode::Down).await;
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("› ○ off"), "{panel}");
    assert!(
        panel.contains("↑↓ select · Enter details · e enable · Esc close"),
        "{panel}"
    );
    // Enter runs `/mcp off` (details).
    rig.code(KeyCode::Enter).await;
    assert_eq!(rig.calls()[2], ("mcp".to_owned(), "off".to_owned()));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_change_runs_the_reload_path_once() {
    let mut rig = rig(true).await;
    rig.submit("/mcp").await;
    rig.code(KeyCode::Down).await;
    assert!(rig.key('e').await);
    assert_eq!(rig.calls()[1], ("mcp".to_owned(), "enable off".to_owned()));
    assert_eq!(*rig.reloads.lock().unwrap(), 1, "one reload");
    let screen = flat(&rig.screen());
    assert!(
        screen.contains("MCP server `off` enabled (config.toml)"),
        "{screen}"
    );
    assert!(
        screen.contains("applied (workers already running keep theirs): mcp: +off"),
        "{screen}"
    );
    // A panel's reload doesn't announce itself like /reload does.
    assert!(!screen.contains("reloading agents"), "{screen}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn errors_reach_the_transcript_and_the_note() {
    let mut rig = rig(true).await;
    rig.submit("/mcp").await;
    rig.submit("/mcp nope").await;
    let screen = flat(&rig.screen());
    assert!(
        screen.contains("/mcp nope: no MCP server `nope` (/mcp lists them)"),
        "{screen}"
    );
    let panel = rig.panel_lines().join("\n");
    assert!(
        panel.contains("no MCP server `nope` (/mcp lists them)"),
        "{panel}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_long_panel_scrolls_past_its_last_row() {
    let mut rig = rig(true).await;
    rig.submit("/config").await;
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("key 0"), "{panel}");
    assert!(!panel.contains("key 39"), "{panel}");
    assert!(panel.contains("↓ 22 more"), "{panel}");
    assert!(panel.contains("↑↓ scroll · Esc close"), "{panel}");
    for _ in 0..30 {
        rig.code(KeyCode::Down).await;
    }
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("key 39"), "{panel}");
    assert!(panel.contains("↑ 22 more"), "{panel}");
    for _ in 0..30 {
        rig.code(KeyCode::Up).await;
    }
    let panel = rig.panel_lines().join("\n");
    assert!(panel.contains("key 0"), "{panel}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn model_routes_roles_to_the_panels_and_refs_to_this_chat() {
    let mut rig = rig(true).await;
    rig.submit("/model roles").await;
    rig.submit("/model worker-backend").await;
    rig.submit("/model worker-backend wf/wf-2 --save").await;
    assert_eq!(
        rig.calls(),
        vec![
            ("model".to_owned(), "roles".to_owned()),
            ("model".to_owned(), "worker-backend".to_owned()),
            (
                "model".to_owned(),
                "worker-backend wf/wf-2 --save".to_owned()
            ),
        ]
    );
    // `provider/model` is still this chat's own model (unknown here).
    rig.submit("/model nobody/none").await;
    assert_eq!(rig.calls().len(), 3);
    assert!(
        rig.screen().contains("unknown model `nobody/none`"),
        "{}",
        rig.screen()
    );
    for name in ["agents", "skills", "config"] {
        rig.submit(&format!("/{name}")).await;
    }
    let names: Vec<String> = rig.calls().into_iter().map(|(n, _)| n).collect();
    assert_eq!(&names[3..], ["agents", "skills", "config"]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_a_control_the_commands_say_so() {
    let mut rig = rig(false).await;
    for (cmd, cli) in [
        ("/mcp", "amp-harness mcp"),
        ("/agents", "amp-harness agents"),
        ("/model roles", "amp-harness models"),
    ] {
        rig.submit(cmd).await;
        let screen = flat(&rig.screen());
        assert!(
            screen.contains(&format!("not available in this session (`{cli}`")),
            "{screen}"
        );
    }
}

#[test]
fn the_status_line_shows_mcp_health() {
    let p = amp_tui::theme::palette();
    let mut data = chrome::sample::data(false);
    let plain = |d: &chrome::ChromeData| chrome::plain(&chrome::render_status(d, &p, 160)[0]);
    assert!(!plain(&data).contains("mcp "), "no hub, no segment");
    data.mcp = Some(HubSummary {
        ready: 3,
        total: 4,
        level: HubLevel::Degraded,
    });
    let line = plain(&data);
    assert!(line.contains("mcp 3/4"), "{line}");
    let colors = rpi_tui::theme().colors;
    assert!(chrome::render_status(&data, &p, 160)[0].contains(&colors.warning.fg("mcp 3/4")));
}
