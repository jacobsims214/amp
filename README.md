# AMP - Agentic Management Platform

AMP bridges OpenCode AI agents with Odoo using a custom project management module with DAG-based workflows.

## Core Concepts

**Static Manager Agent** (`amp-manager`)
Primary OpenCode agent orchestrating work:
- Creates Odoo AMP projects when you start OpenCode in a directory
- Plans work as DAGs of epics/stories/tasks
- Never executes work - delegates to subagents
- Tracks context across sessions
- **Always gets human approval before dispatching**

**Subagent Workers** (`amp-worker`)
Execute assigned tasks:
- Read task instructions from Odoo `amp.task` description field
- Update status via MCP (backlog → ready → in_progress → review → completed)
- Report blockers (moves to blocked state)
- Automatically unblock dependents when completing

**Custom AMP Project Module**
Standalone Odoo 19 module (not extending built-in project):
- `amp.project` - Root container
- `amp.epic` - Large feature
- `amp.story` - User story with dependencies
- `amp.task` - Work item with DAG support
- Custom views optimized for agent workflows
- Real-time dashboard

**MCP Server**
FastAPI wrapper around Odoo XML-RPC API:
- Runs on localhost:8000
- Uses custom `amp.*` models
- All operations as your user

**DAG Workflows**
Directed Acyclic Graph for task dependencies:
- Tasks track dependencies via `dependency_ids`
- `dag_level` computed automatically
- `is_ready` field indicates when dependencies complete
- Parallel execution groups identified

## Quick Start

```bash
# 1. Configure Odoo credentials
cp .env.example .env
# Edit .env with your Odoo details

# 2. Start MCP server
make up

# 3. Verify connection
make status

# 4. OpenCode loads config automatically
```

## Project Structure

```
amp/
├── opencode.json              # OpenCode config
├── docker-compose.yml         # MCP container
├── Makefile                   # Commands
├── .env                       # Odoo credentials
├── prompts/                   # Agent prompts
│   ├── amp-manager.txt
│   └── amp-worker.txt
├── .opencode/                 # OpenCode skills
│   └── planning.md           # Manager planning skill
├── mcp/                       # MCP server
│   ├── src/main.py          # AMP API wrapper
│   └── Dockerfile
├── skills/                    # Reference skills
│   ├── odoo-api/
│   ├── odoo-project/
│   └── data-structures/dag.md
└── odoo-modules/             # Odoo custom modules
    └── amp_project/          # Standalone AMP module
        ├── models/           # amp.project, amp.epic, etc.
        ├── views/            # Custom UI
        └── static/           # CSS/JS
```

## How It Works

1. **Start OpenCode** in directory
2. **Manager** checks for existing AMP project by code
3. **Create project** if new: `POST /projects` → `amp.project`
4. **User requests** work
5. **Manager plans** using DAG skill:
   - Create epic: `POST /epics` → `amp.epic`
   - Create stories: `POST /stories` → `amp.story`
   - Create tasks: `POST /tasks` → `amp.task` (with HTML instructions)
   - Set dependencies: task `dependency_ids` field
6. **Present to human** for approval
7. **After approval**, dispatch ready tasks:
   - Get ready tasks: `GET /projects/{id}/ready-tasks`
   - Dispatch: `POST /tasks/{id}/dispatch`
8. **Workers execute** and update via MCP
9. **Auto-unblock** dependents on completion
10. **Dashboard** shows real-time progress

## AMP Odoo Module

### Models

**amp.project**
```python
name, code, description
state: draft/active/archived
epic_ids (one2many)
epic_count, story_count, task_count
progress_percentage
last_session
```

**amp.epic**
```python
name, project_id
state: backlog/planning/in_progress/completed/cancelled
story_ids (one2many)
dag_json (stored DAG structure)
```

**amp.story**
```python
name, project_id, epic_id
state: backlog/ready/in_progress/review/completed/blocked
dependency_ids (m2m - stories this depends on)
blocked_ids (m2m - stories blocked by this)
is_ready (computed)
task_ids (one2many)
```

**amp.task** (The Instruction)
```python
name, project_id, epic_id, story_id
description (HTML - THIS IS THE AGENT'S INSTRUCTION)
description_text (computed plaintext)
acceptance_criteria
state: backlog/ready/in_progress/review/completed/blocked
dependency_ids (m2m tasks)
dag_level (computed topological level)
dag_critical_path
is_ready (computed)
agent_id (char - assigned agent)
dispatch_time, completion_time
context_data (JSON)
```

### Stages

Tasks/Stories flow through:
1. **Backlog** - Created, not ready
2. **Ready** - Dependencies complete (is_ready=True)
3. **In Progress** - Dispatched to agent
4. **Review** - Submitted for review
5. **Completed** - Done
6. **Blocked** - Cannot proceed

### Views

- **Project Kanban** - Visual overview with progress
- **Epic Form** - With stories list and DAG data
- **Story Form** - With dependencies and tasks
- **Task Form** - Full instructions, dependencies, agent assignment
- **Task Kanban** - Grouped by state for workflow
- **Dashboard** - Real-time stats and progress

## MCP API Endpoints

### Projects
```
POST   /projects
GET    /projects
GET    /projects/{id}
PUT    /projects
GET    /projects/by-code/{code}
GET    /projects/{id}/dashboard
```

### Epics
```
POST   /epics
GET    /epics/{id}
GET    /projects/{id}/epics
PUT    /epics/{id}/dag
```

### Stories
```
POST   /stories
GET    /stories/{id}
GET    /epics/{id}/stories
```

### Tasks
```
POST   /tasks
GET    /tasks/{id}
PUT    /tasks/{id}
POST   /tasks/{id}/dispatch
POST   /tasks/{id}/complete
POST   /tasks/{id}/block
POST   /tasks/{id}/comment
GET    /stories/{id}/tasks
GET    /projects/{id}/tasks
GET    /projects/{id}/ready-tasks
```

## DAG Example

```python
# Dependencies create DAG levels automatically

# Level 0 (no deps)
task_a = create_task(name="Setup", dag_level=0)
task_b = create_task(name="Config", dag_level=0)

# Level 1 (depends on level 0)
task_c = create_task(
    name="Build",
    dag_level=1,
    dependency_ids=[task_a.id, task_b.id]
)

# Level 2 (depends on level 1)
task_d = create_task(
    name="Test",
    dag_level=2,
    dependency_ids=[task_c.id]
)

# Execution order: A, B (parallel) → C → D
```

## Worker Execution Flow

1. **Dispatched** task moves to `in_progress`
2. **Worker reads** task via `GET /tasks/{id}`
3. **Worker executes** based on `description` field
4. **Updates status**:
   - Submit for review: `POST /tasks/{id}/comment` + update state
   - Block: `POST /tasks/{id}/block`
   - Complete: `POST /tasks/{id}/complete`
5. **Completion auto-unblocks** dependent tasks

## Commands

```bash
make up      # Start MCP
make down    # Stop MCP
make logs    # View logs
make status  # Check health
make test    # Test connection
```

## Configuration

**opencode.json**
```json
{
  "agent": {
    "amp-manager": {
      "mode": "primary",
      "prompt": "{file:./prompts/amp-manager.txt}"
    }
  },
  "mcp": {
    "amp-odoo": {
      "type": "remote",
      "url": "http://localhost:8000"
    }
  }
}
```

**.env**
```
ODOO_URL=http://localhost:8069
ODOO_DB=your_db
ODOO_USER=admin
ODOO_PASSWORD=admin
```

## Installation

1. Copy `odoo-modules/amp_project` to Odoo addons path
2. Update `odoo.conf` addons path
3. Restart Odoo
4. Install "AMP Project" from Apps menu
5. Start MCP: `make up`

## Requirements

- Odoo 19
- Docker Desktop
- OpenCode CLI
- Python 3.11+
