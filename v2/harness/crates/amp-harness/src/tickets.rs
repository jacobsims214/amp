//! `amp-harness tickets list` and `tickets get`, with the web UI's state words.

use amp_core::{
    project::ProjectMarker,
    tickets::{AmpTask, FullTaskList, Status, StatusFilter, TicketRow},
    tools::call_as,
};
use clap::{Args, Subcommand};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::conn::{CliError, ConnArgs, connect, print_json};

#[derive(Subcommand)]
pub enum TicketsCommand {
    /// List a project's tickets (open ones by default).
    List(ListArgs),
    /// Show one ticket.
    Get(GetArgs),
}

#[derive(Args)]
pub struct ListArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// Project id (default: project_id from the nearest .amp.json).
    #[arg(long, short)]
    pub project: Option<i64>,
    /// Whose tickets: mine (AMP's default), unassigned, any, or a user's email or id.
    #[arg(long)]
    pub owner: Option<String>,
    /// Only these states, comma-separated: ready, in_progress, waiting, question, scheduled, done,
    /// blocked (waiting+question+scheduled), open, all. Default: open.
    #[arg(long)]
    pub state: Option<StatusFilter>,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct GetArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// Ticket id (a leading `#` is fine).
    pub id: String,
    /// Include the ticket's comments.
    #[arg(long)]
    pub comments: bool,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

pub async fn run(command: TicketsCommand) -> Result<(), CliError> {
    match command {
        TicketsCommand::List(args) => list(args).await,
        TicketsCommand::Get(args) => get(args).await,
    }
}

/// `tickets list --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
pub struct TicketList {
    pub project_id: i64,
    /// AMP's owner view: `mine`, `unassigned`, `any`, or a user.
    pub owner_filter: Option<String>,
    /// The state words shown.
    pub states: Vec<Status>,
    /// Tickets in the owner view before the state filter.
    pub total: usize,
    /// Tickets shown.
    pub count: usize,
    pub tickets: Vec<TicketRow>,
}

fn resolve_project(flag: Option<i64>) -> Result<i64, CliError> {
    if let Some(id) = flag {
        return Ok(id);
    }
    match ProjectMarker::find_from_cwd()? {
        Some(marker) => Ok(marker.project_id),
        None => Err(CliError::usage(
            "no project: pass --project N or run inside a checkout with a .amp.json",
        )),
    }
}

async fn list(args: ListArgs) -> Result<(), CliError> {
    let project_id = resolve_project(args.project)?;
    let filter = match args.state {
        Some(filter) => filter,
        None => "open".parse().expect("valid filter"),
    };
    let settings = args.conn.settings()?;
    let client = connect(&settings).await?;
    let mut params = json!({"project_id": project_id, "format": "full"});
    if let Some(owner) = &args.owner {
        params["owner"] = json!(owner);
    }
    let result: Result<FullTaskList, _> = call_as(&client, "amp_list_tasks", params).await;
    client.close().await;
    let all = result?;
    let rows = all.rows();
    let total = rows.len();
    let tickets: Vec<TicketRow> = rows
        .into_iter()
        .filter(|r| filter.matches(r.status))
        .collect();
    let out = TicketList {
        project_id,
        owner_filter: all.owner_filter.clone(),
        states: Status::ALL
            .into_iter()
            .filter(|s| filter.matches(*s))
            .collect(),
        total,
        count: tickets.len(),
        tickets,
    };
    if args.json {
        return print_json(&out);
    }
    print_list(&out);
    Ok(())
}

fn print_list(list: &TicketList) {
    let owner = list.owner_filter.as_deref().unwrap_or("mine");
    let hidden = list.total - list.count;
    let mut header = format!(
        "Project {} · owner: {owner} · {} ticket{}",
        list.project_id,
        list.count,
        if list.count == 1 { "" } else { "s" }
    );
    if hidden > 0 {
        header.push_str(&format!(" ({hidden} more hidden; --state all shows them)"));
    }
    println!("{header}");
    let width = list
        .tickets
        .iter()
        .map(|t| t.id.to_string().len() + 1)
        .max()
        .unwrap_or(2);
    for status in &list.states {
        let group: Vec<&TicketRow> = list
            .tickets
            .iter()
            .filter(|t| t.status == *status)
            .collect();
        if group.is_empty() {
            continue;
        }
        println!("\n{} ({})", status.label(), group.len());
        for t in group {
            let id = format!("#{}", t.id);
            let who = who(t);
            if who.is_empty() {
                println!("  {id:<width$}  {}", t.name);
            } else {
                println!("  {id:<width$}  {}  [{who}]", t.name);
            }
            let pad = " ".repeat(width + 4);
            if let Some(q) = &t.question {
                println!("{pad}? {}", one_line(q, 100));
            }
            if !t.blocked_by_ids.is_empty() {
                println!("{pad}waits on {}", ids(&t.blocked_by_ids));
            }
            if let Some(at) = &t.start_at {
                println!("{pad}starts {at}");
            }
        }
    }
}

/// Who works a ticket: the agent type, or the person for human tickets.
fn who(t: &TicketRow) -> String {
    match (&t.assigned_to, &t.owner, t.performer.as_deref()) {
        (_, Some(owner), Some("human")) => format!("person: {owner}"),
        (_, None, Some("human")) => "person".to_owned(),
        (Some(agent), _, _) => agent.clone(),
        _ => String::new(),
    }
}

fn ids(ids: &[i64]) -> String {
    ids.iter()
        .map(|id| format!("#{id}"))
        .collect::<Vec<_>>()
        .join(", ")
}

fn one_line(s: &str, max: usize) -> String {
    let flat = s.split_whitespace().collect::<Vec<_>>().join(" ");
    if flat.chars().count() <= max {
        flat
    } else {
        format!("{}…", flat.chars().take(max - 1).collect::<String>())
    }
}

/// `tickets get --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
pub struct TicketDetail {
    #[serde(flatten)]
    pub row: TicketRow,
    pub project_id: Option<i64>,
    pub description: String,
    pub acceptance_criteria: String,
    pub dependency_ids: Vec<i64>,
    pub dispatched_at: Option<String>,
    pub completed_at: Option<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Comment {
    #[serde(default)]
    pub id: Option<i64>,
    #[serde(default)]
    pub author: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub created_at: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct TicketGet {
    pub ticket: TicketDetail,
    /// Present with `--comments`: the most recent comments, oldest first.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub comments: Option<Vec<Comment>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub comments_total: Option<i64>,
}

#[derive(Deserialize)]
struct RawGet {
    task: AmpTask,
    #[serde(default)]
    comments: Option<Vec<Comment>>,
    #[serde(default)]
    comments_total: Option<i64>,
}

async fn get(args: GetArgs) -> Result<(), CliError> {
    let id: i64 = args
        .id
        .trim()
        .trim_start_matches('#')
        .parse()
        .ok()
        .filter(|id| *id > 0)
        .ok_or_else(|| CliError::usage(format!("not a ticket id: {:?}", args.id)))?;
    let settings = args.conn.settings()?;
    let client = connect(&settings).await?;
    let mut params = json!({"task_id": id});
    if args.comments {
        params["include"] = json!(["comments"]);
    }
    let result: Result<RawGet, _> = call_as(&client, "amp_get_task", params).await;
    client.close().await;
    let raw = result?;
    let t = &raw.task;
    let out = TicketGet {
        ticket: TicketDetail {
            row: TicketRow::from(t),
            project_id: t.project_id,
            description: t.description.clone(),
            acceptance_criteria: t.acceptance_criteria.clone(),
            dependency_ids: t.dependency_ids.clone(),
            dispatched_at: t.dispatched_at.clone(),
            completed_at: t.completed_at.clone(),
            created_at: t.created_at.clone(),
            updated_at: t.updated_at.clone(),
        },
        comments: args
            .comments
            .then(|| raw.comments.clone().unwrap_or_default()),
        comments_total: if args.comments {
            raw.comments_total
        } else {
            None
        },
    };
    if args.json {
        return print_json(&out);
    }
    print_ticket(&out);
    Ok(())
}

fn print_ticket(out: &TicketGet) {
    let t = &out.ticket;
    let r = &t.row;
    println!("#{} {}", r.id, r.name);
    println!("  status:     {} ({})", r.status_label, r.state);
    let mut place = Vec::new();
    if let Some(p) = t.project_id {
        place.push(format!("project {p}"));
    }
    if let Some(e) = r.epic_id {
        place.push(format!("epic {e}"));
    }
    if let Some(s) = r.story_id {
        place.push(format!("story {s}"));
    }
    if !place.is_empty() {
        println!("  in:         {}", place.join(" · "));
    }
    if let Some(p) = &r.priority {
        println!("  priority:   {p}");
    }
    let who = who(r);
    if !who.is_empty() {
        println!("  worked by:  {who}");
    }
    if !t.dependency_ids.is_empty() {
        println!("  depends on: {}", ids(&t.dependency_ids));
    }
    if !r.blocked_by_ids.is_empty() {
        println!("  waits on:   {}", ids(&r.blocked_by_ids));
    }
    if let Some(q) = &r.question {
        println!("  question:   {}", one_line(q, 200));
    }
    if let Some(at) = &r.start_at {
        println!("  starts:     {at}");
    }
    for (label, at) in [
        ("dispatched", &t.dispatched_at),
        ("completed", &t.completed_at),
    ] {
        if let Some(at) = at {
            println!("  {label}:{} {at}", " ".repeat(11 - label.len()));
        }
    }
    for (title, body) in [
        ("Description", &t.description),
        ("Acceptance criteria", &t.acceptance_criteria),
    ] {
        if !body.trim().is_empty() {
            println!("\n{title}");
            for line in body.trim_end().lines() {
                println!("  {line}");
            }
        }
    }
    if let Some(comments) = &out.comments {
        let total = out.comments_total.unwrap_or(comments.len() as i64);
        println!("\nComments ({} of {total})", comments.len());
        for c in comments {
            let at = c.created_at.as_deref().unwrap_or("");
            println!("  --- {} {at}", c.author);
            for line in c.body.trim_end().lines() {
                println!("  {line}");
            }
        }
    }
}
