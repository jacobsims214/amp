//! M2-2 (#2093): the role registry built from the embedded roster, each role's tool policy (allow
//! and deny cases per role), and the before-tool hook inside a real rpi agent loop: a denied
//! call never runs and the model sees the denial as a tool error.

use std::{
    collections::VecDeque,
    fs,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use amp_core::{
    assets::{Assets, OverrideDirs},
    models::{ModelsConfig, ModelsFile},
    policy::{
        DISPATCH_TOOL, DenialReason, Policy, QUESTION_TOOL, ToolPolicy, resolve_read_tool_path,
        resolve_symlinks, resolve_tool_path,
    },
    roles::{RoleError, RoleRegistry},
};
use rpi_agent::{AgentContext, AgentMessage, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::{
    event_stream::create_assistant_message_event_stream,
    types::{
        Api, AssistantMessage, AssistantMessageEvent, Content, DoneReason, ErrorReason, Message,
        StopReason, Tool, ToolCall, ToolCallType, Usage, UserContent, UserMessage,
    },
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

const ROOT: &str = "/work/repo/.worktrees/42";

fn registry() -> RoleRegistry {
    RoleRegistry::from_assets(&Assets::embedded().unwrap()).unwrap()
}

fn policy(id: &str) -> ToolPolicy {
    registry().get(id).unwrap().tool_policy(ROOT)
}

fn allowed(p: &ToolPolicy, tool: &str, args: Value) -> bool {
    p.check(tool, &args).is_ok()
}

fn bash(p: &ToolPolicy, cmd: &str) -> bool {
    allowed(p, "bash", json!({ "command": cmd }))
}

fn write(p: &ToolPolicy, path: &str) -> bool {
    allowed(p, "write", json!({ "path": path, "content": "x" }))
}

// ------------------------------------------------------------------------------------------------
// The registry.
// ------------------------------------------------------------------------------------------------

#[test]
fn registry_has_every_role_with_prompt_skills_and_model_key() {
    let reg = registry();
    let ids: Vec<&str> = reg.roles().map(|r| r.id.as_str()).collect();
    assert_eq!(ids.len(), 11, "{ids:?}");
    let manager = reg.manager();
    assert_eq!(manager.id, "amp-manager");
    assert!(manager.primary);
    // #2367: nothing preloaded; the manager reads amp-planning when it plans.
    assert!(manager.skills.is_empty());
    assert_eq!(manager.model_role, "manager");

    let expect_skills: &[(&str, &[&str])] = &[
        ("amp-worker-backend", &["amp-execution"]),
        ("amp-worker-frontend", &["amp-execution"]),
        ("amp-worker-docs", &["amp-execution"]),
        ("amp-designer", &["amp-execution"]),
        ("amp-media", &["amp-execution"]),
        ("amp-researcher", &[]),
        ("amp-github-researcher", &["github-research"]),
        ("amp-reviewer", &["amp-review", "code-reviewer"]),
        ("amp-github-reviewer", &["amp-review", "github-pr-review"]),
        ("amp-kb-curator", &["amp-kb-curator"]),
    ];
    for (id, skills) in expect_skills {
        let role = reg.get(id).unwrap();
        assert!(role.is_child(), "{id}");
        assert_eq!(role.skills, *skills, "{id}");
        assert!(!role.prompt.is_empty(), "{id}");
        assert!(role.frontmatter_model.is_some(), "{id}");
        assert_eq!(role.model_role, id.strip_prefix("amp-").unwrap());
    }

    // Every role's model key resolves in the shipped models.toml.
    let models = ModelsConfig::from_files(ModelsFile::defaults(), None, "test", &|_| None).unwrap();
    for role in reg.roles() {
        models
            .resolve_role(&role.model_role)
            .unwrap_or_else(|e| panic!("{}: {e}", role.id));
    }
}

#[test]
fn lookup_accepts_amp_agent_ids_and_short_names() {
    let reg = registry();
    for name in [
        "amp:amp-worker-backend",
        "amp-worker-backend",
        "worker-backend",
    ] {
        assert_eq!(reg.get(name).unwrap().id, "amp-worker-backend", "{name}");
    }
    assert!(reg.get("general").is_none());
}

#[test]
fn manager_dispatches_exactly_the_ten_workers() {
    let reg = registry();
    let mut ids: Vec<&str> = reg
        .dispatchable(reg.manager())
        .iter()
        .map(|r| r.id.as_str())
        .collect();
    ids.sort();
    assert_eq!(
        ids,
        [
            "amp-designer",
            "amp-github-researcher",
            "amp-github-reviewer",
            "amp-kb-curator",
            "amp-media",
            "amp-researcher",
            "amp-reviewer",
            "amp-worker-backend",
            "amp-worker-docs",
            "amp-worker-frontend",
        ]
    );
    let worker = reg.get("amp-worker-backend").unwrap();
    assert!(reg.dispatchable(worker).is_empty());
}

/// #2234 (D11): `dispatch: true` lets the manager run a subagent whose id doesn't match its
/// `task` rules, in `dispatchable` and in the before-tool hook alike; a manager rule that names
/// the agent with deny still wins.
#[test]
fn dispatch_true_opts_a_subagent_in_unless_the_manager_names_it_denied() {
    let base = std::path::Path::new(env!("CARGO_TARGET_TMPDIR")).join("roles-dispatch");
    let _ = std::fs::remove_dir_all(&base);
    let agents = base.join("user/agents");
    std::fs::create_dir_all(&agents).unwrap();
    let agent = |id: &str, extra: &str| {
        std::fs::write(
            agents.join(format!("{id}.md")),
            format!("---\ndescription: {id}\nmode: subagent\n{extra}---\nDo {id}.\n"),
        )
        .unwrap();
    };
    agent("sql-expert", "dispatch: true\n");
    agent("helper", "");
    agent("blocked", "dispatch: true\n");
    let load = || {
        let assets = Assets::load(&OverrideDirs {
            user: Some(base.join("user")),
            repo: None,
        })
        .unwrap();
        RoleRegistry::from_assets(&assets).unwrap()
    };
    let reg = load();
    let ids: Vec<&str> = reg
        .dispatchable(reg.manager())
        .iter()
        .map(|r| r.id.as_str())
        .collect();
    assert!(
        ids.contains(&"sql-expert") && ids.contains(&"blocked"),
        "{ids:?}"
    );
    assert!(!ids.contains(&"helper"), "{ids:?}");
    let hook = reg.manager().tool_policy(ROOT);
    assert!(allowed(
        &hook,
        "amp_run_ticket",
        json!({"task_id": 1, "agent_id": "amp:sql-expert"})
    ));
    assert!(!allowed(
        &hook,
        "amp_run_ticket",
        json!({"task_id": 1, "agent_id": "amp:helper"})
    ));

    // A manager override that denies `blocked` by name keeps it out; the catch-all doesn't.
    let manager = Assets::embedded_agent_text("amp-manager").unwrap().replace(
        "    \"amp-*\": allow\n",
        "    \"amp-*\": allow\n    \"blocked\": deny\n",
    );
    assert!(manager.contains("\"blocked\": deny"));
    std::fs::write(agents.join("amp-manager.md"), manager).unwrap();
    let reg = load();
    let ids: Vec<&str> = reg
        .dispatchable(reg.manager())
        .iter()
        .map(|r| r.id.as_str())
        .collect();
    assert!(
        ids.contains(&"sql-expert") && !ids.contains(&"blocked"),
        "{ids:?}"
    );
    let hook = reg.manager().tool_policy(ROOT);
    assert!(!allowed(
        &hook,
        "amp_run_ticket",
        json!({"task_id": 1, "agent_id": "blocked"})
    ));

    // Bad values are errors naming the agent.
    agent("bad", "dispatch: yes please\n");
    let assets = Assets::load(&OverrideDirs {
        user: Some(base.join("user")),
        repo: None,
    })
    .unwrap();
    let e = RoleRegistry::from_assets(&assets).unwrap_err().to_string();
    assert!(
        e.contains("agent bad: dispatch must be true or false"),
        "{e}"
    );
}

// ------------------------------------------------------------------------------------------------
// The manager.
// ------------------------------------------------------------------------------------------------

#[test]
fn manager_policy() {
    let p = policy("amp-manager");
    assert!(!p.is_child());

    // bash: read-only git and ls only (catch-all first, exceptions after).
    for ok in [
        "git status",
        "git status --short",
        "git diff HEAD~1",
        "git log --oneline -5",
        "ls",
        "ls -la v2",
        "git status && git log -1",
        "ls 2>/dev/null",
        "git log 2>&1",
        // #2363: the user's session had these denied and retried them.
        "git status 2>&1 | head -20; echo \"---\"; pwd; ls -la",
        "git status --short --branch 2>&1 | head -20; echo \"---LOG---\"; git log --oneline -5 2>&1 | head -5",
        "git log --oneline | head -5",
        "git show HEAD --stat",
        "git rev-parse --show-toplevel",
        "git ls-files | wc -l",
        "grep -o amp_[a-z_]* notes.md | sort -u",
        "cat README.md | grep -n TODO",
        "grep -rn amp_ src | head -20",
        "tail -n 20 notes.md",
        "date -u",
        "pwd",
    ] {
        assert!(bash(&p, ok), "manager should run: {ok}");
    }
    for denied in [
        "rm -rf target",
        "cargo build",
        "git push origin main",
        "git commit -m x",
        "git status && rm -rf x",
        "ls; curl https://example.com",
        "git log | sh",
        // Every command of a pipeline is checked: `sh`, `xargs`, `tee` are not on the list.
        "git log | xargs rm",
        "cat x | tee y",
        "grep -rl x . | xargs sed -i s/a/b/",
        // #2363: read-only pipes are fine, following a file forever is not.
        "tail -f log.txt",
        "tail -n 5 -f log.txt",
        "tail -F x",
        "tail --follow=name x",
        "cat x > y",
        "echo hi >> notes.md",
        "sort -o x y",
        "sort -u x -o y",
        "uniq a b",
        "find . -delete",
        "git branch -D main",
        "mkdir -p .opencode/agent",
        "git diff $(rm -rf x)",
        "git log `id`",
        "git log > notes.txt",
        "lsof -i",
        // #2101: `$'...'` and `#` comments used to make the lexer read each of these as one
        // `ls ...` command while bash also ran `rm -rf x`.
        "ls $'\\'';rm -rf x #'",
        "ls $'\\'' && rm -rf x #'",
        "ls $'\\'' | rm -rf x #'",
        "ls #'\nrm -rf x\n#'",
        "ls #'\nls && rm -rf x\n#'",
        "ls #'\nls | rm -rf x\n#'",
        "git status \"\nrm -rf x\n\"",
        "ls 'unclosed",
        "ls <> created-file",
        // #2140: git's `--output` writes the command's output to any path, so an allowed
        // `git log` could write e.g. ~/.zshrc with fully controlled content.
        "git log --output=x",
        "git diff --output x",
        "git log -1 --format='alias ls=rm%n' --output=~/.zshrc",
        "git show --output=x",
        "git log --outp=x",
        "git log '--output'=x",
        "git log --format=x --output-directory d",
        "git log -o x",
        "git log ${X:---output=x}",
        "git log {--output=x,}",
    ] {
        assert!(!bash(&p, denied), "manager must not run: {denied}");
    }
    for ok in [
        "ls -la",
        "git status && git log --oneline",
        "ls -la # what's here",
    ] {
        assert!(bash(&p, ok), "manager should run: {ok}");
    }
    assert_eq!(
        p.check("bash", &json!({"command": "ls $'\\'';rm -rf x #'"}))
            .unwrap_err()
            .reason,
        DenialReason::Unparseable
    );
    let d = p
        .check("bash", &json!({"command": "cargo build"}))
        .unwrap_err();
    assert_eq!(d.reason, DenialReason::Deny);
    assert_eq!(d.rule.as_ref().unwrap().pattern, "*");
    for ok in [
        "git diff --stat",
        "git log --format='--output=x' -1",
        "git log -- --output",
        "git log HEAD@{1} -1",
    ] {
        assert!(bash(&p, ok), "manager should run: {ok}");
    }
    let d = p
        .check(
            "bash",
            &json!({"command": "git log -1 --format=x --output=~/.zshrc"}),
        )
        .unwrap_err();
    assert_eq!(d.reason, DenialReason::Redirect);
    assert!(d.to_string().contains("--output"), "{d}");
    assert_eq!(
        p.check("bash", &json!({"command": "git log {--output=x,}"}))
            .unwrap_err()
            .reason,
        DenialReason::Unparseable
    );
    assert_eq!(
        p.check("bash", &json!({"command": "git log > x"}))
            .unwrap_err()
            .reason,
        DenialReason::Redirect
    );

    // edit/write: only `.amp.json` in the working directory (#2363: the amp-init skill writes the
    // project marker), denied everywhere else.
    for path in [
        "src/main.rs",
        ".opencode/agent/x.md",
        "opencode.json",
        "/etc/hosts",
        "sub/.amp.json",
        "../.amp.json",
        ".amp.json.bak",
    ] {
        assert!(!write(&p, path), "{path}");
        assert!(
            !allowed(&p, "edit", json!({"path": path, "edits": []})),
            "{path}"
        );
    }
    assert!(write(&p, ".amp.json"));
    assert!(p.exposes("edit") && p.exposes("write"));
    assert!(p.exposes("bash"));

    // web and todos: denied.
    assert!(!allowed(&p, "webfetch", json!({"url": "https://x.dev"})));
    assert!(!allowed(&p, "websearch", json!({"query": "x"})));
    assert!(!allowed(&p, "todowrite", json!({})));
    assert!(!p.exposes("webfetch") && !p.exposes("websearch") && !p.exposes("todowrite"));

    // Dispatch: amp-* only, with or without the amp: prefix.
    assert!(p.exposes(DISPATCH_TOOL));
    for agent in [
        "amp:amp-worker-backend",
        "amp-reviewer",
        "amp:amp-kb-curator",
    ] {
        assert!(
            allowed(&p, DISPATCH_TOOL, json!({"task_id": 1, "agent_id": agent})),
            "{agent}"
        );
    }
    // The opencode-only subagents the overrides removed, and short names, don't match `amp-*`.
    for agent in ["general", "explore", "worker-backend"] {
        let d = p
            .check(DISPATCH_TOOL, &json!({"task_id": 1, "agent_id": agent}))
            .unwrap_err();
        assert_eq!(d.permission, "task");
        assert_eq!(d.reason, DenialReason::Deny, "{agent}");
    }
    assert_eq!(
        p.check(DISPATCH_TOOL, &json!({"task_id": 1}))
            .unwrap_err()
            .reason,
        DenialReason::BadArguments
    );

    // Reads and AMP tools: allowed.
    assert!(allowed(&p, "read", json!({"path": "README.md"})));
    assert!(allowed(&p, "grep", json!({"pattern": "x"})));
    assert!(allowed(&p, "amp_kb_write", json!({})));
    assert!(allowed(&p, "amp_create_tasks", json!({})));
}

/// #2145 (review of PR #74): the manager's bash allows are word-bounded (`git diff *`, not
/// `git diff*`). `git diff*` also matched `git difftool`, whose `-x/--extcmd` and `-t/--tool` run
/// any command, so one allowed-looking command was unrestricted bash. The lexer can't see it:
/// there is no substitution, redirect or `--output`.
#[test]
fn manager_bash_allows_stop_at_the_word() {
    let p = policy("amp-manager");
    for denied in [
        "git difftool",
        "git difftool HEAD~1",
        "git difftool -x 'sh -c id'",
        "git difftool -x sh",
        "git difftool --extcmd=sh",
        "git difftool --extcmd 'rm -rf x'",
        "git difftool -t vimdiff",
        "git difftool --tool=x HEAD",
        "git difftool -y --tool x",
        "git difftool --dir-diff",
        // Other git commands and binaries that the old prefix rules matched.
        "git diff-tree HEAD",
        "git diff-index HEAD",
        "git diff-files",
        "git statusx",
        "git status-foo",
        "git logx",
        "git log-foo",
        "lsof -i",
        "lsblk",
    ] {
        let d = p
            .check("bash", &json!({ "command": denied }))
            .expect_err(denied);
        assert_eq!(d.reason, DenialReason::Deny, "{denied}");
    }
    // The backstop rule decides difftool.
    let d = p
        .check("bash", &json!({"command": "git difftool -x sh"}))
        .unwrap_err();
    assert_eq!(d.rule.as_ref().unwrap().pattern, "git difftool*");
    // Bare and argumented forms stay allowed: a trailing ` *` also matches the bare command.
    for ok in [
        "git status",
        "git status --short",
        "git diff",
        "git diff HEAD~1 --stat",
        "git diff --stat",
        "git log",
        "git log --oneline -5",
        "ls",
        "ls -la v2",
    ] {
        assert!(bash(&p, ok), "manager should run: {ok}");
    }
    // Found in the same audit: bash splits an unquoted `$IFS` (or any unquoted variable) at run
    // time, and a glob can expand to a file named `--output=…`, so both could hide `--output`
    // from the lexer.
    for denied in [
        "git log -1 $IFS--output=x",
        "git log -1 x${IFS}--output=x",
        "git diff HEAD $IFS--output=x",
        "git diff *",
        "git log -1 [-]-output=x",
    ] {
        let d = p
            .check("bash", &json!({ "command": denied }))
            .expect_err(denied);
        assert_eq!(d.reason, DenialReason::Unparseable, "{denied}");
    }
    for ok in ["git diff -- '*.rs'", "git log -1 \"$X\"", "ls *.md"] {
        assert!(bash(&p, ok), "manager should run: {ok}");
    }
}

/// The same rules as opencode evaluates them: opencode has no shell lexer, so its manager's
/// bash rules (v2/plugins/opencode/agents.json, generated from the same source) must stop
/// difftool, redirects, `--output` and mkdir outside `.opencode/` on their own. Matching is
/// `policy::wildcard_match` with last match wins, which copies opencode's `Wildcard.match` and
/// `Permission.evaluate` (`findLast`).
#[test]
fn opencode_manager_bash_rules_hold_without_the_lexer() {
    let file = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../plugins/opencode/agents.json");
    let agents: Value = serde_json::from_str(&fs::read_to_string(&file).unwrap()).unwrap();
    let manager = agents
        .as_array()
        .unwrap()
        .iter()
        .find(|a| a["id"] == "amp-manager")
        .expect("amp-manager in agents.json");
    let mut bash = serde_json::Map::new();
    for rule in manager["permissions"].as_array().unwrap() {
        if rule["action"] == "shell" {
            let pattern = rule["resource"].as_str().unwrap().to_string();
            // A later rule for the same pattern replaces it; none repeat today.
            assert!(!bash.contains_key(&pattern), "repeated pattern {pattern}");
            bash.insert(pattern, rule["effect"].clone());
        }
    }
    let rules = Policy::parse(Some(&json!({ "bash": bash }))).unwrap();
    let allows = |cmd: &str| rules.evaluate("bash", cmd).allows();
    for ok in [
        "git status",
        "git status --short",
        "git diff",
        "git diff HEAD~1 --stat",
        "git log",
        "git log --oneline -5",
        "ls",
        "ls -la v2",
        "mkdir -p .opencode/agent",
        "mkdir -p .opencode/skills/new-skill",
        // #2363: opencode checks each command of a pipeline on its own.
        "head -20",
        "tail -n 5 notes.md",
        "cat README.md",
        "grep -rn TODO src",
        "wc -l",
        "git show HEAD --stat",
        "pwd",
    ] {
        assert!(allows(ok), "opencode manager should run: {ok}");
    }
    for denied in [
        "git difftool",
        "git difftool -x 'sh -c id'",
        "git difftool --extcmd=sh",
        "git difftool -t vim",
        "git difftool --tool=x HEAD",
        "git diff-tree HEAD",
        "git statusx",
        "git logx",
        "lsof -i",
        // opencode matches a command's whole source text, redirect included.
        "git diff > x",
        "git diff>~/.zshrc",
        "git log 2>&1 >x",
        "git log --output=x",
        "git log --outp=x",
        "git diff --output x",
        // `.opencode/*` crosses spaces, `/` and `..`, so it needs these denies.
        "mkdir -p .opencode/x /etc/y",
        "mkdir -p .opencode/../../x",
        "mkdir -p .opencode/x$IFS/etc/y",
        "mkdir -p .opencode/x${IFS}/etc/y",
        "mkdir -p .opencode/x\t/etc/y",
        "mkdir -p .opencode/x>~/.zshrc",
        "mkdir /etc/x",
        "rm -rf x",
        "tail -f log.txt",
        "tail -n 5 -f log.txt",
        "tail --follow=name x",
        "cat x > y",
        "echo x >> notes.md",
        "git show --output=x",
        "xargs rm",
    ] {
        assert!(!allows(denied), "opencode manager must not run: {denied}");
    }
}

// ------------------------------------------------------------------------------------------------
// The workers.
// ------------------------------------------------------------------------------------------------

/// Per-role expectations, straight from each agent file's `permission` block.
struct Expect {
    id: &'static str,
    edit: bool,
    webfetch: bool,
    /// `websearch` has no rule in the implementers' files, so opencode's default (allow) applies.
    websearch: bool,
}

const WORKERS: [Expect; 10] = [
    Expect {
        id: "amp-worker-backend",
        edit: true,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-worker-frontend",
        edit: true,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-worker-docs",
        edit: true,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-designer",
        edit: true,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-media",
        edit: true,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-researcher",
        edit: false,
        webfetch: true,
        websearch: true,
    },
    Expect {
        id: "amp-github-researcher",
        edit: false,
        webfetch: false,
        websearch: false,
    },
    Expect {
        id: "amp-reviewer",
        edit: false,
        webfetch: false,
        websearch: false,
    },
    Expect {
        id: "amp-github-reviewer",
        edit: false,
        webfetch: false,
        websearch: false,
    },
    Expect {
        id: "amp-kb-curator",
        edit: true,
        webfetch: false,
        websearch: false,
    },
];

#[test]
fn every_worker_policy_matches_its_frontmatter() {
    for e in &WORKERS {
        let p = policy(e.id);
        let id = e.id;
        assert!(p.is_child(), "{id}");

        // bash: every worker has `bash: allow`, so the whole script runs (pipes, redirects and
        // substitution included).
        for cmd in [
            "cargo test --workspace",
            "git commit -m 'x' && git push origin harness-m2",
            "make build > build.log 2>&1",
            "echo $(git rev-parse HEAD)",
        ] {
            assert!(bash(&p, cmd), "{id}: {cmd}");
        }
        assert!(p.exposes("bash"), "{id}");

        // edit/write inside the worktree: per the file.
        assert_eq!(write(&p, "src/lib.rs"), e.edit, "{id} write");
        assert_eq!(
            write(&p, &format!("{ROOT}/docs/a.md")),
            e.edit,
            "{id} abs write"
        );
        assert_eq!(
            allowed(&p, "edit", json!({"path": "src/lib.rs", "edits": []})),
            e.edit,
            "{id} edit"
        );
        assert_eq!(p.exposes("edit"), e.edit, "{id} exposes edit");
        assert_eq!(p.exposes("write"), e.edit, "{id} exposes write");

        // Outside the worktree: `external_directory` defaults to ask, which is deny.
        for outside in ["/tmp/x", "../other/x", "/work/repo/v2/x"] {
            let d = p
                .check("write", &json!({"path": outside, "content": ""}))
                .unwrap_err();
            assert_eq!(d.permission, "external_directory", "{id} {outside}");
            assert_eq!(d.reason, DenialReason::Ask, "{id} {outside}");
        }

        // Web.
        assert_eq!(
            allowed(&p, "webfetch", json!({"url": "https://docs.rs"})),
            e.webfetch,
            "{id} webfetch"
        );
        assert_eq!(
            allowed(&p, "websearch", json!({"query": "rmcp"})),
            e.websearch,
            "{id} websearch"
        );

        // Never: questions, dispatch.
        let d = p.check(QUESTION_TOOL, &json!({})).unwrap_err();
        assert_eq!(d.reason, DenialReason::ChildQuestion, "{id}");
        assert!(d.to_string().contains("amp_block_task"), "{id}");
        let d = p
            .check(
                DISPATCH_TOOL,
                &json!({"task_id": 1, "agent_id": "amp:amp-worker-docs"}),
            )
            .unwrap_err();
        assert_eq!(d.reason, DenialReason::ChildDispatch, "{id}");
        assert!(
            !p.exposes(QUESTION_TOOL) && !p.exposes(DISPATCH_TOOL),
            "{id}"
        );

        // AMP tools and reads: allowed.
        assert!(
            allowed(&p, "amp_complete_task", json!({"task_id": 1})),
            "{id}"
        );
        assert!(allowed(&p, "read", json!({"path": "Cargo.toml"})), "{id}");
    }
}

#[test]
fn todowrite_is_denied_wherever_the_file_says_so() {
    for id in [
        "amp-manager",
        "amp-worker-backend",
        "amp-worker-frontend",
        "amp-worker-docs",
        "amp-designer",
        "amp-media",
        "amp-researcher",
        "amp-reviewer",
    ] {
        assert!(!allowed(&policy(id), "todowrite", json!({})), "{id}");
    }
}

#[test]
fn denial_text_names_the_role_rule_and_says_nobody_will_be_asked() {
    let p = policy("amp-reviewer");
    let text = p
        .check("write", &json!({"path": "src/a.rs", "content": ""}))
        .unwrap_err()
        .to_string();
    assert!(text.contains("amp-reviewer"), "{text}");
    assert!(text.contains("edit `src/a.rs`"), "{text}");
    assert!(text.contains("\"*\": deny"), "{text}");
    assert!(text.contains("never asks for approval"), "{text}");
}

// ------------------------------------------------------------------------------------------------
// Overrides.
// ------------------------------------------------------------------------------------------------

fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("roles")
        .join(name);
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(dir.join("agents")).unwrap();
    dir
}

fn load_with_repo_agent(name: &str, file: &str, text: &str) -> Result<RoleRegistry, RoleError> {
    let repo = scratch(name);
    fs::write(repo.join("agents").join(file), text).unwrap();
    let assets = Assets::load(&OverrideDirs {
        user: None,
        repo: Some(repo),
    })
    .unwrap();
    RoleRegistry::from_assets(&assets)
}

#[test]
fn override_files_set_skills_and_policy_and_bad_ones_name_the_role() {
    let reg = load_with_repo_agent(
        "ok",
        "amp-worker-docs.md",
        "---\ndescription: docs\nmode: subagent\nskills: [amp-kb, git-workflow]\npermission:\n  edit:\n    \"*\": deny\n    \"docs/**\": allow\n---\nPrompt.\n",
    )
    .unwrap();
    let role = reg.get("worker-docs").unwrap();
    assert_eq!(role.skills, ["amp-kb", "git-workflow"]);
    let p = role.tool_policy(ROOT);
    assert!(write(&p, "docs/guide.md"));
    assert!(!write(&p, "src/main.rs"));
    assert!(p.exposes("edit"));

    let err = load_with_repo_agent(
        "missing-skill",
        "amp-worker-docs.md",
        "---\ndescription: docs\nskills: [no-such-skill]\n---\nPrompt.\n",
    )
    .unwrap_err();
    assert!(
        matches!(&err, RoleError::MissingSkill { role, skill } if role == "amp-worker-docs" && skill == "no-such-skill"),
        "{err}"
    );

    let err = load_with_repo_agent(
        "bad-policy",
        "amp-worker-docs.md",
        "---\ndescription: docs\npermission:\n  bash: sometimes\n---\nPrompt.\n",
    )
    .unwrap_err();
    assert!(err.to_string().contains("amp-worker-docs"), "{err}");
    assert!(err.to_string().contains("sometimes"), "{err}");
}

// ------------------------------------------------------------------------------------------------
// The hook inside a real rpi agent loop.
// ------------------------------------------------------------------------------------------------

/// Records every call that actually executes.
struct Recorder {
    schema: Tool,
    ran: Arc<Mutex<Vec<Value>>>,
}

#[async_trait::async_trait]
impl AgentTool for Recorder {
    fn schema(&self) -> &Tool {
        &self.schema
    }
    fn label(&self) -> &str {
        &self.schema.name
    }
    async fn execute(
        &self,
        _id: &str,
        params: Value,
        _signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, rpi_agent::AgentError> {
        self.ran.lock().unwrap().push(params);
        Ok(AgentToolResult::text("ran"))
    }
}

fn recorder(name: &str, ran: &Arc<Mutex<Vec<Value>>>) -> Arc<dyn AgentTool> {
    Arc::new(Recorder {
        schema: Tool {
            name: name.into(),
            description: name.into(),
            parameters: json!({"type": "object", "additionalProperties": true}).into(),
            constrained_sampling: None,
        },
        ran: Arc::clone(ran),
    })
}

fn assistant(content: Vec<Content>, stop: StopReason) -> AssistantMessage {
    AssistantMessage {
        role: rpi_ai::types::AssistantRole,
        content,
        api: Api::Other("mock".into()),
        provider: "mock".into(),
        model: "mock".into(),
        response_model: None,
        response_id: None,
        usage: Usage::zero(),
        stop_reason: stop,
        deferred: None,
        error_message: None,
        raw_stop_reason: None,
        end_turn: None,
        timestamp: 0,
    }
}

fn call(id: &str, name: &str, args: Value) -> Content {
    Content::ToolCall(ToolCall {
        kind: ToolCallType,
        id: id.into(),
        name: name.into(),
        arguments: args,
        thought_signature: None,
        namespace: None,
    })
}

/// A scripted model: one assistant message per LLM call.
fn scripted(messages: Vec<AssistantMessage>) -> rpi_agent::StreamFn {
    let queue = Arc::new(Mutex::new(VecDeque::from(messages)));
    rpi_agent::stream_fn(move |_model, _ctx, _opts| {
        let (mut prod, stream) = create_assistant_message_event_stream();
        let next = queue.lock().unwrap().pop_front();
        tokio::spawn(async move {
            match next {
                Some(message) => {
                    prod.push(AssistantMessageEvent::Start {
                        partial: Arc::new(message.clone()),
                    });
                    let reason = if message.stop_reason == StopReason::ToolUse {
                        DoneReason::ToolUse
                    } else {
                        DoneReason::Stop
                    };
                    prod.push(AssistantMessageEvent::Done { reason, message });
                }
                None => {
                    let error = AssistantMessage::terminal(
                        Api::Other("mock".into()),
                        "mock",
                        "mock",
                        StopReason::Error,
                        "script exhausted",
                        0,
                    );
                    prod.push(AssistantMessageEvent::Error {
                        reason: ErrorReason::Error,
                        error,
                    });
                }
            }
        });
        stream
    })
}

fn loop_config(hook: rpi_agent::BeforeToolCall) -> rpi_agent::AgentLoopConfig {
    rpi_agent::AgentLoopConfig {
        model: rpi_ai::Model::new(
            "mock",
            "mock",
            Api::Other("mock".into()),
            "mock",
            "https://x.invalid",
        ),
        convert_to_llm: Arc::new(|messages: Vec<AgentMessage>| {
            let out: Vec<Message> = messages
                .into_iter()
                .filter_map(|m| match m {
                    AgentMessage::User(u) => Some(Message::User(u)),
                    AgentMessage::Assistant(a) => Some(Message::Assistant(a)),
                    AgentMessage::ToolResult(t) => Some(Message::ToolResult(t)),
                    AgentMessage::Custom(_) => None,
                })
                .collect();
            Box::pin(async move { out })
        }),
        transform_context: None,
        get_api_key: None,
        should_stop_after_turn: None,
        prepare_next_turn: None,
        after_tool_results: None,
        get_steering_messages: None,
        get_follow_up_messages: None,
        before_tool_call: Some(hook),
        after_tool_call: None,
        tool_execution: rpi_agent::ToolExecutionMode::Sequential,
        thinking_level: rpi_ai::types::ThinkingLevel::Off,
        api_key: None,
        timeout: None,
        max_retries: None,
        max_retry_delay: None,
        cache_retention: rpi_ai::provider::CacheRetention::default(),
        session_id: None,
        signal: CancellationToken::new(),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn denied_calls_never_run_and_come_back_as_tool_errors() {
    // A reviewer (edit: deny) tries to write, run bash, ask a question and dispatch; only bash is
    // allowed.
    let policy = Arc::new(policy("amp-reviewer"));
    let ran = Arc::new(Mutex::new(Vec::new()));
    let tools = vec![
        recorder("write", &ran),
        recorder("bash", &ran),
        recorder(QUESTION_TOOL, &ran),
        recorder(DISPATCH_TOOL, &ran),
    ];
    let script = scripted(vec![
        assistant(
            vec![
                call("c1", "write", json!({"path": "src/a.rs", "content": "x"})),
                call("c2", "bash", json!({"command": "git diff main"})),
                call("c3", QUESTION_TOOL, json!({"question": "may I?"})),
                call(
                    "c4",
                    DISPATCH_TOOL,
                    json!({"task_id": 7, "agent_id": "amp:amp-worker-backend"}),
                ),
            ],
            StopReason::ToolUse,
        ),
        assistant(vec![Content::text("done")], StopReason::Stop),
    ]);
    let context = AgentContext {
        system_prompt: String::new(),
        messages: Vec::new(),
        tools,
    };
    let prompt = AgentMessage::User(UserMessage::new(UserContent::Text("go".into()), 0));
    let (collector, _events) = rpi_agent::CollectorEmitter::new();
    let messages = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        rpi_agent::run_agent_loop(
            vec![prompt],
            context,
            loop_config(policy.before_tool_call()),
            Arc::new(collector),
            script,
        ),
    )
    .await
    .expect("the loop finished without waiting on anyone")
    .unwrap();

    // Only the allowed bash call executed.
    let ran = ran.lock().unwrap().clone();
    assert_eq!(ran, vec![json!({"command": "git diff main"})]);

    // The model saw three errors and one result, then answered (the run continued).
    let results: Vec<_> = messages
        .iter()
        .filter_map(|m| match m {
            AgentMessage::ToolResult(t) => Some(t),
            _ => None,
        })
        .collect();
    assert_eq!(results.len(), 4);
    let text = |id: &str| {
        let t = results.iter().find(|t| t.tool_call_id == id).unwrap();
        let body: String = t
            .content
            .iter()
            .filter_map(|c| match c {
                Content::Text(t) => Some(t.text.clone()),
                _ => None,
            })
            .collect();
        (t.is_error, body)
    };
    let (err, body) = text("c1");
    assert!(
        err && body.contains("Denied by the amp-reviewer tool policy"),
        "{body}"
    );
    let (err, body) = text("c2");
    assert!(!err && body == "ran", "{body}");
    let (err, body) = text("c3");
    assert!(err && body.contains("not available to workers"), "{body}");
    let (err, body) = text("c4");
    assert!(err && body.contains("cannot dispatch"), "{body}");
    assert!(matches!(
        messages.last(),
        Some(AgentMessage::Assistant(a)) if a.stop_reason == StopReason::Stop
    ));
}

#[test]
fn filter_tools_drops_what_a_role_can_never_use() {
    let ran = Arc::new(Mutex::new(Vec::new()));
    let all = || {
        vec![
            recorder("read", &ran),
            recorder("bash", &ran),
            recorder("edit", &ran),
            recorder("write", &ran),
            recorder(QUESTION_TOOL, &ran),
            recorder(DISPATCH_TOOL, &ran),
            recorder("amp_get_task", &ran),
        ]
    };
    let names = |p: &ToolPolicy| -> Vec<String> {
        p.filter_tools(all())
            .iter()
            .map(|t| t.schema().name.clone())
            .collect()
    };
    assert_eq!(
        names(&policy("amp-manager")),
        // #2363: edit/write stay for `.amp.json` only (the amp-init skill).
        [
            "read",
            "bash",
            "edit",
            "write",
            QUESTION_TOOL,
            DISPATCH_TOOL,
            "amp_get_task"
        ]
    );
    assert_eq!(
        names(&policy("amp-worker-backend")),
        ["read", "bash", "edit", "write", "amp_get_task"]
    );
    assert_eq!(
        names(&policy("amp-reviewer")),
        ["read", "bash", "amp_get_task"]
    );
}

// ------------------------------------------------------------------------------------------------
// External MCP servers: the `mcp` permission (#2240, H7).
// ------------------------------------------------------------------------------------------------

/// A worker whose frontmatter has `mcp` rules (catch-all first, last match wins).
const MCP_WORKER: &str = "---\ndescription: mcp\nmode: subagent\npermission:\n  edit: allow\n  bash: allow\n  mcp:\n    \"*\": deny\n    \"github/*\": allow\n    \"github/delete_*\": deny\n    \"github/delete_branch\": allow\n    \"fs/*\": allow\n    \"fs/write.*\": ask\n  mcp__fs__nuke: deny\n---\nPrompt.\n";

fn mcp_policy(name: &str) -> ToolPolicy {
    let reg = load_with_repo_agent(name, "amp-worker-docs.md", MCP_WORKER).unwrap();
    let p = reg.get("worker-docs").unwrap().tool_policy(ROOT);
    // As the hub names them: `write.file` is sanitised to `write_file`.
    p.learn_mcp_subjects([
        ("mcp__github__get_issue", "github/get_issue"),
        ("mcp__github__delete_repo", "github/delete_repo"),
        ("mcp__github__delete_branch", "github/delete_branch"),
        ("mcp__fs__read", "fs/read"),
        ("mcp__fs__write_file", "fs/write.file"),
        ("mcp__fs__nuke", "fs/nuke"),
        ("mcp__db__query", "db/query"),
    ]);
    p
}

#[test]
fn mcp_permission_judges_server_slash_tool_last_match_wins() {
    let p = mcp_policy("mcp-rules");
    let ok = |t: &str| p.check(t, &json!({})).is_ok();
    assert!(ok("mcp__github__get_issue"));
    assert!(!ok("mcp__github__delete_repo"));
    // A later, narrower allow wins over the earlier deny.
    assert!(ok("mcp__github__delete_branch"));
    assert!(ok("mcp__fs__read"));
    // The subject is the server's own tool name (`write.file`), not the sanitised one; ask = deny.
    let d = p.check("mcp__fs__write_file", &json!({})).unwrap_err();
    assert_eq!(d.reason, DenialReason::Ask);
    assert_eq!(
        (d.permission.as_str(), d.subject.as_str()),
        ("mcp", "fs/write.file")
    );
    // The catch-all denies a server no rule names.
    assert!(!ok("mcp__db__query"));
    // A key named after the tool itself still applies, after `mcp`.
    let d = p.check("mcp__fs__nuke", &json!({})).unwrap_err();
    assert_eq!(d.permission, "mcp__fs__nuke");
    // A name the hub never reported is read from the name: `<server>/<rest>`.
    assert_eq!(
        p.mcp_subject("mcp__github__list_prs").unwrap(),
        "github/list_prs"
    );
    assert!(ok("mcp__github__list_prs"));
    assert!(!ok("mcp__github__delete_tag"));
    assert_eq!(p.mcp_subject("amp_get_task"), None);
    // AMP's own tools are not `mcp`.
    assert!(ok("amp_get_task"));
}

#[test]
fn mcp_denial_is_visible_and_names_the_rule() {
    let p = mcp_policy("mcp-denial");
    let text = p
        .check("mcp__github__delete_repo", &json!({}))
        .unwrap_err()
        .to_string();
    assert!(
        text.contains("Denied by the amp-worker-docs tool policy: mcp `github/delete_repo` matches the rule \"github/delete_*\": deny"),
        "{text}"
    );
    assert!(text.contains("never asks for approval"), "{text}");
    let text = p
        .check("mcp__fs__write_file", &json!({}))
        .unwrap_err()
        .to_string();
    assert!(text.contains("(ask is treated as deny)"), "{text}");
}

#[test]
fn tools_a_role_can_never_use_are_left_out_of_its_list() {
    let p = mcp_policy("mcp-filter");
    let ran = Arc::new(Mutex::new(Vec::new()));
    let names: Vec<String> = p
        .filter_tools(vec![
            recorder("read", &ran),
            recorder("amp_get_task", &ran),
            recorder("mcp__github__get_issue", &ran),
            recorder("mcp__github__delete_repo", &ran),
            recorder("mcp__github__delete_branch", &ran),
            recorder("mcp__fs__read", &ran),
            recorder("mcp__fs__write_file", &ran),
            recorder("mcp__fs__nuke", &ran),
            recorder("mcp__db__query", &ran),
        ])
        .iter()
        .map(|t| t.schema().name.clone())
        .collect();
    assert_eq!(
        names,
        [
            "read",
            "amp_get_task",
            "mcp__github__get_issue",
            "mcp__github__delete_branch",
            "mcp__fs__read"
        ]
    );

    // `mcp: deny` hides every external tool and nothing else.
    let reg = load_with_repo_agent(
        "mcp-none",
        "amp-worker-docs.md",
        "---\ndescription: none\nmode: subagent\npermission:\n  mcp: deny\n---\nPrompt.\n",
    )
    .unwrap();
    let p = reg.get("worker-docs").unwrap().tool_policy(ROOT);
    assert!(!p.exposes("mcp__github__get_issue"));
    assert!(p.check("mcp__github__get_issue", &json!({})).is_err());
    assert!(p.exposes("amp_get_task") && p.exposes("read"));
}

#[test]
fn with_no_mcp_rule_every_shipped_role_sees_every_server() {
    let reg = registry();
    for role in reg.roles() {
        let p = role.tool_policy(ROOT);
        p.learn_mcp_subjects([("mcp__github__delete_repo", "github/delete_repo")]);
        assert!(
            p.exposes("mcp__github__delete_repo")
                && p.check("mcp__github__delete_repo", &json!({})).is_ok(),
            "{}",
            role.id
        );
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_denied_mcp_call_never_runs_and_the_model_sees_why() {
    let policy = Arc::new(mcp_policy("mcp-loop"));
    let ran = Arc::new(Mutex::new(Vec::new()));
    let tools = vec![
        recorder("mcp__github__get_issue", &ran),
        recorder("mcp__github__delete_repo", &ran),
    ];
    let script = scripted(vec![
        assistant(
            vec![
                call("c1", "mcp__github__delete_repo", json!({"repo": "x"})),
                call("c2", "mcp__github__get_issue", json!({"n": 1})),
            ],
            StopReason::ToolUse,
        ),
        assistant(vec![Content::text("done")], StopReason::Stop),
    ]);
    let context = AgentContext {
        system_prompt: String::new(),
        messages: Vec::new(),
        tools,
    };
    let prompt = AgentMessage::User(UserMessage::new(UserContent::Text("go".into()), 0));
    let (collector, _events) = rpi_agent::CollectorEmitter::new();
    let messages = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        rpi_agent::run_agent_loop(
            vec![prompt],
            context,
            loop_config(policy.before_tool_call()),
            Arc::new(collector),
            script,
        ),
    )
    .await
    .expect("the loop finished")
    .unwrap();
    assert_eq!(ran.lock().unwrap().clone(), vec![json!({"n": 1})]);
    let denied = messages
        .iter()
        .find_map(|m| match m {
            AgentMessage::ToolResult(t) if t.tool_call_id == "c1" => Some(t),
            _ => None,
        })
        .unwrap();
    let body: String = denied
        .content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.clone()),
            _ => None,
        })
        .collect();
    assert!(
        denied.is_error && body.contains("mcp `github/delete_repo`"),
        "{body}"
    );
}

// ------------------------------------------------------------------------------------------------
// Path resolution (#2141): the policy must judge the file rpi's tools will really touch.
// ------------------------------------------------------------------------------------------------

/// A fresh directory under this crate's target dir (never /tmp: RUST DISK RULE, and on macOS /tmp
/// is itself a symlink).
fn path_scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("paths")
        .join(name);
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).unwrap();
    dir
}

fn home() -> PathBuf {
    resolve_symlinks(&dirs::home_dir().expect("a home dir"))
}

/// The policy's resolution is a copy of rpi-tools' (whose resolver is private to its env), so
/// compare the two on every rule: `@`, unicode spaces, `~`, `file://`, `..`, and the read tools'
/// typographic variants.
#[tokio::test]
async fn policy_resolves_paths_exactly_like_rpi_tools() {
    let root = path_scratch("rpi-equivalence").join("wt");
    fs::create_dir_all(root.join("docs")).unwrap();
    // Files only the read tools' variant lookup finds.
    fs::write(root.join("docs/log 2\u{202F}PM.txt"), "").unwrap();
    fs::write(root.join("docs/it\u{2019}s.md"), "").unwrap();
    fs::write(root.join("docs/cafe\u{0301}.md"), "").unwrap();
    let env = rpi_tools::OsExecutionEnv::with_cwd(root.clone());
    let edit_paths = [
        "src/a.rs",
        "./src/../src/a.rs",
        "@src/a.rs",
        "@@src/a.rs",
        "@../../x",
        "~",
        "~/",
        "~/.ssh/authorized_keys",
        "~/../.opencode/x",
        "~root/x",
        "x/~/y",
        "file:///etc/hosts",
        "file://docs/a.md",
        "file:///C:/x",
        "@file:///etc/hosts",
        "~/file:///x",
        "a\u{00A0}b\u{3000}c.txt",
        "\u{2003}@x",
        "/abs/../../../x",
        "../../../../../../../../../../../../../../x",
        "",
        ".",
    ];
    for raw in edit_paths {
        let rpi = rpi_tools::resolve_tool_path(&env, raw, None).await.unwrap();
        assert_eq!(
            resolve_tool_path(&root, raw),
            PathBuf::from(rpi),
            "edit path {raw:?}"
        );
    }
    let read_paths = [
        "docs/log 2 PM.txt",
        "docs/log 2 pm.txt",
        "docs/it's.md",
        "docs/caf\u{00E9}.md",
        "@docs/it's.md",
        "docs/missing it's 2 AM.txt",
        "~/x",
    ];
    for raw in edit_paths.iter().chain(read_paths.iter()) {
        let rpi = rpi_tools::resolve_read_tool_path(&env, raw, None)
            .await
            .unwrap();
        assert_eq!(
            resolve_read_tool_path(&root, raw),
            PathBuf::from(rpi),
            "read path {raw:?}"
        );
    }
}

fn edit_policy(root: &Path) -> ToolPolicy {
    // Like the manager's own-config rule: everything denied but `.opencode/**`.
    let p = Policy::parse(Some(&json!({
        "edit": {"*": "deny", ".opencode/**": "allow"},
        "read": {"*": "deny", "docs/**": "allow", ".opencode/**": "allow"},
    })))
    .unwrap();
    ToolPolicy::new("amp-x", true, p, root)
}

fn edit_subject(p: &ToolPolicy, raw: &str) -> Result<(), (String, String)> {
    p.check("write", &json!({"path": raw, "content": "x"}))
        .map_err(|d| (d.permission.clone(), d.subject.clone()))
}

#[test]
fn tilde_at_and_file_urls_are_classified_where_rpi_writes() {
    let base = path_scratch("tilde-at-file");
    let root = base.join("wt");
    fs::create_dir_all(root.join(".opencode")).unwrap();
    let p = edit_policy(&root);
    let real_root = resolve_symlinks(&root);
    let real_base = resolve_symlinks(&base);
    let external = |subject: PathBuf| {
        Err((
            "external_directory".to_string(),
            subject.display().to_string(),
        ))
    };

    // Inside, allowed: plain, `@`-prefixed, unicode space-free, and as a file:// URL.
    assert_eq!(edit_subject(&p, ".opencode/x"), Ok(()));
    assert_eq!(edit_subject(&p, "@.opencode/x"), Ok(()));
    let url = format!("file://{}/.opencode/x", root.display());
    assert_eq!(edit_subject(&p, &url), Ok(()));
    assert_eq!(edit_subject(&p, &format!("@{url}")), Ok(()));
    // `~` and `file:` used to be read as directory names inside the root.
    assert_eq!(
        edit_subject(&p, "~/.opencode/x"),
        external(home().join(".opencode/x"))
    );
    assert_eq!(
        edit_subject(&p, "~/.ssh/authorized_keys"),
        external(home().join(".ssh/authorized_keys"))
    );
    // Used to collapse to `.opencode/x` and pass the allow rule.
    let above_home = resolve_symlinks(&home().join("..")).join(".opencode/x");
    assert_eq!(edit_subject(&p, "~/../.opencode/x"), external(above_home));
    assert_eq!(
        edit_subject(&p, "file:///.opencode/x"),
        external(PathBuf::from("/.opencode/x"))
    );
    // `@..` climbs out of the worktree, so the `.opencode/x` of a sibling is outside.
    assert_eq!(
        edit_subject(&p, "@../other/.opencode/x"),
        external(real_base.join("other/.opencode/x"))
    );
    // `@../wt/...` comes back in.
    assert_eq!(edit_subject(&p, "@../wt/.opencode/x"), Ok(()));
    // Inside but denied: the subject is relative to the root.
    assert_eq!(
        edit_subject(&p, "@src/a.rs"),
        Err(("edit".into(), "src/a.rs".into()))
    );
    // A worker with `edit: allow` still needs external_directory for `~/`.
    let worker = registry()
        .get("amp-worker-backend")
        .unwrap()
        .tool_policy(&real_root);
    let d = worker
        .check(
            "write",
            &json!({"path": "~/.ssh/authorized_keys", "content": ""}),
        )
        .unwrap_err();
    assert_eq!(d.permission, "external_directory");
    assert_eq!(d.reason, DenialReason::Ask);
    assert!(
        worker
            .check("write", &json!({"path": "@src/a.rs", "content": ""}))
            .is_ok()
    );
}

#[cfg(unix)]
#[test]
fn symlinks_are_followed_before_the_rules_see_the_path() {
    use std::os::unix::fs::symlink;

    let base = path_scratch("symlinks");
    let root = base.join("wt");
    let outside = base.join("outside");
    fs::create_dir_all(root.join(".opencode")).unwrap();
    fs::create_dir_all(root.join("docs")).unwrap();
    fs::create_dir_all(&outside).unwrap();
    fs::write(outside.join("secret.txt"), "s").unwrap();
    let real_outside = resolve_symlinks(&outside);
    // A directory link that leaves the worktree.
    symlink(&outside, root.join(".opencode/escape")).unwrap();
    // A file link and a dangling link (writing through it creates its target) out of it.
    symlink(outside.join("secret.txt"), root.join(".opencode/file.md")).unwrap();
    symlink(outside.join("new.md"), root.join(".opencode/dangling.md")).unwrap();
    // A relative dangling link whose target is under a directory that doesn't exist yet.
    symlink("../../outside/deep/new.md", root.join(".opencode/rel.md")).unwrap();
    // Links that stay inside: the rules see the real path.
    symlink(root.join(".opencode"), root.join("alias")).unwrap();
    symlink("../src", root.join(".opencode/to-src")).unwrap();
    // The worktree itself reached through a link.
    let root_link = base.join("wt-link");
    symlink(&root, &root_link).unwrap();
    // A link loop.
    symlink("loop-b", root.join(".opencode/loop-a")).unwrap();
    symlink("loop-a", root.join(".opencode/loop-b")).unwrap();

    let p = edit_policy(&root);
    let external = |subject: PathBuf| {
        Err((
            "external_directory".to_string(),
            subject.display().to_string(),
        ))
    };
    assert_eq!(
        edit_subject(&p, ".opencode/escape/x"),
        external(real_outside.join("x"))
    );
    assert_eq!(
        edit_subject(&p, ".opencode/escape/a/b/c"),
        external(real_outside.join("a/b/c"))
    );
    assert_eq!(
        edit_subject(&p, ".opencode/file.md"),
        external(real_outside.join("secret.txt"))
    );
    assert_eq!(
        edit_subject(&p, ".opencode/dangling.md"),
        external(real_outside.join("new.md"))
    );
    assert_eq!(
        edit_subject(&p, ".opencode/rel.md"),
        external(real_outside.join("deep/new.md"))
    );
    // `..` is collapsed before the link is seen, as rpi does: this is `.opencode/x`.
    assert_eq!(edit_subject(&p, ".opencode/escape/../x"), Ok(()));
    // Inside links: `alias/x` is really `.opencode/x`; `.opencode/to-src/a.rs` is `src/a.rs`.
    assert_eq!(edit_subject(&p, "alias/x"), Ok(()));
    assert_eq!(
        edit_subject(&p, ".opencode/to-src/a.rs"),
        Err(("edit".into(), "src/a.rs".into()))
    );
    // A loop ends (the tool itself fails with ELOOP, so the verdict doesn't matter).
    let _ = edit_subject(&p, ".opencode/loop-a");
    // A policy bound to the linked root sees the same inside paths.
    let via_link = edit_policy(&root_link);
    assert_eq!(edit_subject(&via_link, ".opencode/x"), Ok(()));
    assert_eq!(
        edit_subject(&via_link, ".opencode/escape/x"),
        external(real_outside.join("x"))
    );

    // Reads use the same resolution: a link out of an allowed directory is not allowed.
    let read = |raw: &str| p.check("read", &json!({ "path": raw }));
    assert!(read("docs/a.md").is_ok());
    assert!(read("@docs/a.md").is_ok());
    assert!(read("alias/x").is_ok());
    let d = read(".opencode/escape/secret.txt").unwrap_err();
    assert_eq!(d.permission, "read");
    assert_eq!(
        d.subject,
        real_outside.join("secret.txt").display().to_string()
    );
    assert!(read(".opencode/file.md").is_err());
    assert!(read("~/.opencode/x").is_err());
    for tool in ["grep", "find", "ls"] {
        let checks = p
            .checks(tool, &json!({"path": ".opencode/escape"}))
            .unwrap();
        assert_eq!(checks.len(), 1, "{tool}");
        assert_eq!(
            checks[0].subject,
            real_outside.display().to_string(),
            "{tool}"
        );
    }
}
