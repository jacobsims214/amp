"""
AMP MCP Server - Odoo AMP Project Management API Wrapper
FastAPI-based MCP server for AMP custom project module
"""

from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel
from typing import List, Optional, Dict, Any
import xmlrpc.client
import os
from dotenv import load_dotenv

load_dotenv()

app = FastAPI(title="AMP MCP Server", version="2.0.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Configuration
ODOO_URL = os.getenv("ODOO_URL", "http://host.docker.internal:8069")
ODOO_DB = os.getenv("ODOO_DB", "odoo_db")
ODOO_USER = os.getenv("ODOO_USER", "admin")
ODOO_PASSWORD = os.getenv("ODOO_PASSWORD", "admin")


class OdooClient:
    """XML-RPC client for Odoo API"""

    def __init__(self):
        self.url = ODOO_URL
        self.db = ODOO_DB
        self.username = ODOO_USER
        self.password = ODOO_PASSWORD
        self.uid = None
        self.common = None
        self.models = None
        self._authenticate()

    def _authenticate(self):
        """Authenticate with Odoo"""
        try:
            self.common = xmlrpc.client.ServerProxy(f"{self.url}/xmlrpc/2/common")
            self.uid = self.common.authenticate(
                self.db, self.username, self.password, {}
            )
            if not self.uid:
                raise Exception("Authentication failed")
            self.models = xmlrpc.client.ServerProxy(f"{self.url}/xmlrpc/2/object")
        except Exception as e:
            print(f"Odoo authentication error: {e}")
            raise

    def execute(self, model: str, method: str, *args, **kwargs):
        """Execute Odoo model method"""
        try:
            return self.models.execute_kw(
                self.db, self.uid, self.password, model, method, args, kwargs
            )
        except xmlrpc.client.Fault as e:
            print(f"Odoo error: {e.faultString}")
            raise HTTPException(status_code=400, detail=str(e.faultString))
        except Exception as e:
            print(f"Execute error: {e}")
            raise HTTPException(status_code=500, detail=str(e))


# Lazy client initialization
_odoo_client = None


def get_odoo_client():
    """Get or create Odoo client (lazy initialization)"""
    global _odoo_client
    if _odoo_client is None:
        _odoo_client = OdooClient()
    return _odoo_client


# ============== Request/Response Models ==============


class CreateProjectRequest(BaseModel):
    name: str
    code: Optional[str] = None
    description: Optional[str] = ""


class UpdateProjectRequest(BaseModel):
    project_id: int
    name: Optional[str] = None
    description: Optional[str] = None
    state: Optional[str] = None


class CreateEpicRequest(BaseModel):
    name: str
    project_id: int
    description: Optional[str] = ""
    priority: Optional[str] = "1"


class CreateStoryRequest(BaseModel):
    name: str
    project_id: int
    epic_id: int
    description: Optional[str] = ""
    acceptance_criteria: Optional[str] = ""
    priority: Optional[str] = "1"
    planned_hours: Optional[float] = 0.0


class CreateTaskRequest(BaseModel):
    name: str
    project_id: int
    epic_id: Optional[int] = None
    story_id: Optional[int] = None
    description: Optional[str] = ""  # HTML instructions
    acceptance_criteria: Optional[str] = ""
    priority: Optional[str] = "1"
    planned_hours: Optional[float] = 0.0
    dependency_ids: Optional[List[int]] = []
    dag_level: Optional[int] = 0


class UpdateTaskRequest(BaseModel):
    task_id: int
    name: Optional[str] = None
    description: Optional[str] = None
    state: Optional[str] = None
    agent_id: Optional[str] = None
    context_data: Optional[str] = None


class DispatchTaskRequest(BaseModel):
    task_id: int
    agent_id: str


class AddCommentRequest(BaseModel):
    task_id: int
    body: str


class UpdateDAGRequest(BaseModel):
    epic_id: int
    dag_json: str


# ============== Health Check ==============


@app.get("/health")
def health_check():
    """Health check endpoint"""
    try:
        client = get_odoo_client()
        version = client.common.version()
        return {
            "status": "healthy",
            "odoo_connected": True,
            "odoo_version": version.get("server_version", "unknown"),
            "amp_module": "amp.project",
        }
    except Exception as e:
        return {"status": "unhealthy", "odoo_connected": False, "error": str(e)}


# ============== Project Endpoints ==============


@app.post("/projects")
def create_project(request: CreateProjectRequest):
    """Create a new AMP project"""
    vals = {"name": request.name, "description": request.description, "state": "active"}
    if request.code:
        vals["code"] = request.code

    project_id = get_odoo_client().execute("amp.project", "create", vals)
    return {"project_id": project_id, "name": request.name}


@app.get("/projects")
def list_projects(limit: int = 50, offset: int = 0):
    """List all AMP projects"""
    projects = get_odoo_client().execute(
        "amp.project",
        "search_read",
        [[["state", "in", ["draft", "active"]]]],
        {
            "fields": [
                "name",
                "code",
                "state",
                "epic_count",
                "task_count",
                "progress_percentage",
            ],
            "limit": limit,
            "offset": offset,
        },
    )
    return {"projects": projects}


@app.get("/projects/{project_id}")
def get_project(project_id: int):
    """Get project details with full hierarchy"""
    projects = get_odoo_client().execute(
        "amp.project",
        "read",
        [[project_id]],
        {
            "fields": [
                "name",
                "code",
                "description",
                "state",
                "epic_count",
                "story_count",
                "task_count",
                "completed_task_count",
                "blocked_task_count",
                "progress_percentage",
                "active_agent_count",
                "last_session",
                "epic_ids",
            ]
        },
    )
    if not projects:
        raise HTTPException(status_code=404, detail="Project not found")
    return {"project": projects[0]}


@app.put("/projects")
def update_project(request: UpdateProjectRequest):
    """Update project fields"""
    vals = {}
    if request.name:
        vals["name"] = request.name
    if request.description:
        vals["description"] = request.description
    if request.state:
        vals["state"] = request.state

    if vals:
        get_odoo_client().execute("amp.project", "write", [request.project_id], vals)

    return {"project_id": request.project_id, "updated": vals}


# ============== Epic Endpoints ==============


@app.post("/epics")
def create_epic(request: CreateEpicRequest):
    """Create a new epic"""
    vals = {
        "name": request.name,
        "project_id": request.project_id,
        "description": request.description,
        "priority": request.priority,
        "state": "backlog",
    }

    epic_id = get_odoo_client().execute("amp.epic", "create", vals)
    return {"epic_id": epic_id, "name": request.name, "project_id": request.project_id}


@app.get("/epics/{epic_id}")
def get_epic(epic_id: int):
    """Get epic with stories"""
    epics = get_odoo_client().execute(
        "amp.epic",
        "read",
        [[epic_id]],
        {
            "fields": [
                "name",
                "code",
                "description",
                "project_id",
                "state",
                "story_count",
                "task_count",
                "progress_percentage",
                "story_ids",
                "dag_json",
            ]
        },
    )
    if not epics:
        raise HTTPException(status_code=404, detail="Epic not found")
    return {"epic": epics[0]}


@app.get("/projects/{project_id}/epics")
def get_project_epics(project_id: int):
    """Get all epics for a project"""
    epics = get_odoo_client().execute(
        "amp.epic",
        "search_read",
        [[["project_id", "=", project_id]]],
        {"fields": ["name", "state", "story_count", "progress_percentage", "priority"]},
    )
    return {"epics": epics}


@app.put("/epics/{epic_id}/dag")
def update_epic_dag(epic_id: int, request: UpdateDAGRequest):
    """Store DAG structure in epic"""
    get_odoo_client().execute("amp.epic", "write", [epic_id], {"dag_json": request.dag_json})
    return {"epic_id": epic_id, "dag_updated": True}


# ============== Story Endpoints ==============


@app.post("/stories")
def create_story(request: CreateStoryRequest):
    """Create a new story under an epic"""
    vals = {
        "name": request.name,
        "project_id": request.project_id,
        "epic_id": request.epic_id,
        "description": request.description,
        "acceptance_criteria": request.acceptance_criteria,
        "priority": request.priority,
        "planned_hours": request.planned_hours,
        "state": "backlog",
    }

    story_id = get_odoo_client().execute("amp.story", "create", vals)
    return {"story_id": story_id, "name": request.name, "epic_id": request.epic_id}


@app.get("/stories/{story_id}")
def get_story(story_id: int):
    """Get story with tasks"""
    stories = get_odoo_client().execute(
        "amp.story",
        "read",
        [[story_id]],
        {
            "fields": [
                "name",
                "description",
                "acceptance_criteria",
                "project_id",
                "epic_id",
                "state",
                "is_ready",
                "task_count",
                "progress_percentage",
                "task_ids",
                "dependency_ids",
                "blocked_ids",
            ]
        },
    )
    if not stories:
        raise HTTPException(status_code=404, detail="Story not found")
    return {"story": stories[0]}


@app.get("/epics/{epic_id}/stories")
def get_epic_stories(epic_id: int):
    """Get all stories for an epic"""
    stories = get_odoo_client().execute(
        "amp.story",
        "search_read",
        [[["epic_id", "=", epic_id]]],
        {"fields": ["name", "state", "is_ready", "task_count", "progress_percentage"]},
    )
    return {"stories": stories}


# ============== Task Endpoints ==============


@app.post("/tasks")
def create_task(request: CreateTaskRequest):
    """Create a new task with DAG dependencies"""
    vals = {
        "name": request.name,
        "project_id": request.project_id,
        "description": request.description,
        "acceptance_criteria": request.acceptance_criteria,
        "priority": request.priority,
        "planned_hours": request.planned_hours,
        "dag_level": request.dag_level,
        "state": "backlog",
    }

    if request.epic_id:
        vals["epic_id"] = request.epic_id
    if request.story_id:
        vals["story_id"] = request.story_id
    if request.dependency_ids:
        vals["dependency_ids"] = [(6, 0, request.dependency_ids)]

    task_id = get_odoo_client().execute("amp.task", "create", vals)
    return {"task_id": task_id, "name": request.name}


@app.get("/tasks/{task_id}")
def get_task(task_id: int):
    """Get task details with dependencies"""
    tasks = get_odoo_client().execute(
        "amp.task",
        "read",
        [[task_id]],
        {
            "fields": [
                "name",
                "description",
                "description_text",
                "acceptance_criteria",
                "project_id",
                "epic_id",
                "story_id",
                "state",
                "is_ready",
                "dag_level",
                "dag_critical_path",
                "priority",
                "planned_hours",
                "actual_hours",
                "agent_id",
                "agent_session",
                "dispatch_time",
                "completion_time",
                "context_data",
                "dependency_ids",
                "blocked_ids",
                "dependency_count",
                "blocked_count",
            ]
        },
    )
    if not tasks:
        raise HTTPException(status_code=404, detail="Task not found")
    return {"task": tasks[0]}


@app.put("/tasks/{task_id}")
def update_task(task_id: int, request: UpdateTaskRequest):
    """Update task fields"""
    vals = {}
    if request.name:
        vals["name"] = request.name
    if request.description:
        vals["description"] = request.description
    if request.state:
        vals["state"] = request.state
    if request.agent_id:
        vals["agent_id"] = request.agent_id
    if request.context_data:
        vals["context_data"] = request.context_data

    if vals:
        get_odoo_client().execute("amp.task", "write", [task_id], vals)

    return {"task_id": task_id, "updated": vals}


@app.post("/tasks/{task_id}/dispatch")
def dispatch_task(task_id: int, request: DispatchTaskRequest):
    """Dispatch task to an agent"""
    get_odoo_client().execute("amp.task", "action_dispatch", [task_id, request.agent_id])
    return {"task_id": task_id, "dispatched_to": request.agent_id}


@app.post("/tasks/{task_id}/complete")
def complete_task(task_id: int):
    """Mark task as completed"""
    get_odoo_client().execute("amp.task", "action_complete", [task_id])
    return {"task_id": task_id, "state": "completed"}


@app.post("/tasks/{task_id}/block")
def block_task(task_id: int, reason: str = ""):
    """Block a task"""
    get_odoo_client().execute("amp.task", "action_block", [task_id, reason])
    return {"task_id": task_id, "state": "blocked"}


@app.get("/stories/{story_id}/tasks")
def get_story_tasks(story_id: int):
    """Get all tasks for a story"""
    tasks = get_odoo_client().execute(
        "amp.task",
        "search_read",
        [[["story_id", "=", story_id]]],
        {
            "fields": [
                "name",
                "state",
                "is_ready",
                "dag_level",
                "dag_critical_path",
                "agent_id",
                "priority",
            ]
        },
    )
    return {"tasks": tasks}


@app.get("/projects/{project_id}/tasks")
def get_project_tasks(project_id: int, state: Optional[str] = None):
    """Get tasks for a project, optionally filtered by state"""
    domain = [["project_id", "=", project_id]]
    if state:
        domain.append(["state", "=", state])

    tasks = get_odoo_client().execute(
        "amp.task",
        "search_read",
        [domain],
        {
            "fields": [
                "name",
                "state",
                "is_ready",
                "story_id",
                "agent_id",
                "dag_level",
                "dag_critical_path",
                "priority",
            ]
        },
    )
    return {"tasks": tasks}


# ============== Ready Tasks (for Dispatch) ==============


@app.get("/projects/{project_id}/ready-tasks")
def get_ready_tasks(project_id: int):
    """Get all tasks ready to be dispatched (dependencies complete)"""
    tasks = get_odoo_client().execute(
        "amp.task",
        "search_read",
        [
            [
                ["project_id", "=", project_id],
                ["state", "=", "ready"],
                ["is_ready", "=", True],
            ]
        ],
        {
            "fields": [
                "name",
                "description_text",
                "acceptance_criteria",
                "story_id",
                "dag_level",
                "planned_hours",
            ]
        },
    )
    return {"ready_tasks": tasks}


# ============== Comments / Messages ==============


@app.post("/tasks/{task_id}/comment")
def add_task_comment(task_id: int, request: AddCommentRequest):
    """Add comment to task"""
    get_odoo_client().execute(
        "amp.task",
        "message_post",
        [task_id],
        {"body": request.body, "message_type": "comment"},
    )
    return {"task_id": task_id, "comment_added": True}


# ============== Dashboard Data ==============


@app.get("/projects/{project_id}/dashboard")
def get_project_dashboard(project_id: int):
    """Get dashboard data for real-time monitoring"""
    dashboard = get_odoo_client().execute("amp.dashboard", "get_project_dashboard", [project_id])
    return dashboard


# ============== Find Project by Code ==============


@app.get("/projects/by-code/{code}")
def get_project_by_code(code: str):
    """Find project by its code (for directory matching)"""
    projects = get_odoo_client().execute(
        "amp.project",
        "search_read",
        [[["code", "=", code]]],
        {"fields": ["name", "state", "epic_count", "task_count"], "limit": 1},
    )
    if not projects:
        raise HTTPException(status_code=404, detail="Project not found")
    return {"project": projects[0]}


# ============== Knowledge Base Endpoints ==============


class CreateKBEntryRequest(BaseModel):
    title: str
    content: str
    project_id: int
    epic_id: Optional[int] = None
    story_id: Optional[int] = None
    task_id: Optional[int] = None
    entry_type: Optional[str] = "context"
    tags: Optional[List[str]] = []
    created_by_agent: Optional[str] = None


class SearchKBRequest(BaseModel):
    query: str
    project_id: Optional[int] = None
    entry_type: Optional[str] = None
    limit: Optional[int] = 20


@app.post("/kb")
def create_kb_entry(request: CreateKBEntryRequest):
    """Create a knowledge base entry"""
    vals = {
        "title": request.title,
        "content": request.content,
        "project_id": request.project_id,
        "entry_type": request.entry_type,
    }

    if request.epic_id:
        vals["epic_id"] = request.epic_id
    if request.story_id:
        vals["story_id"] = request.story_id
    if request.task_id:
        vals["task_id"] = request.task_id
    if request.tags:
        vals["tags"] = ",".join(request.tags)
    if request.created_by_agent:
        vals["created_by_agent"] = request.created_by_agent

    entry_id = get_odoo_client().execute("amp.knowledge.entry", "create", vals)
    return {"entry_id": entry_id, "title": request.title}


@app.post("/kb/search")
def search_kb(request: SearchKBRequest):
    """Search knowledge base entries"""
    domain = [
        "|",
        "|",
        "|",
        ["title", "ilike", request.query],
        ["content_text", "ilike", request.query],
        ["tags", "ilike", request.query],
    ]

    if request.project_id:
        domain.append(["project_id", "=", request.project_id])
    if request.entry_type:
        domain.append(["entry_type", "=", request.entry_type])

    entries = get_odoo_client().execute(
        "amp.knowledge.entry",
        "search_read",
        [domain],
        {
            "fields": [
                "title",
                "entry_type",
                "content_text",
                "project_id",
                "epic_id",
                "story_id",
                "task_id",
                "created_by_agent",
                "create_date",
                "tags",
            ],
            "limit": request.limit,
            "order": "create_date desc",
        },
    )
    return {"entries": entries}


@app.get("/kb/entries/{entry_id}")
def get_kb_entry(entry_id: int):
    """Get a specific KB entry"""
    entries = get_odoo_client().execute(
        "amp.knowledge.entry",
        "read",
        [[entry_id]],
        {
            "fields": [
                "title",
                "content",
                "content_text",
                "entry_type",
                "project_id",
                "epic_id",
                "story_id",
                "task_id",
                "created_by_agent",
                "create_date",
                "tags",
            ]
        },
    )
    if not entries:
        raise HTTPException(status_code=404, detail="KB entry not found")
    return {"entry": entries[0]}


@app.get("/projects/{project_id}/kb")
def get_project_kb(project_id: int, limit: int = 20):
    """Get all KB entries for a project"""
    entries = get_odoo_client().execute(
        "amp.knowledge.entry",
        "search_read",
        [[["project_id", "=", project_id]]],
        {
            "fields": [
                "title",
                "entry_type",
                "content_text",
                "epic_id",
                "story_id",
                "task_id",
                "created_by_agent",
                "create_date",
            ],
            "limit": limit,
            "order": "create_date desc",
        },
    )
    return {"entries": entries}


@app.get("/tasks/{task_id}/kb")
def get_task_kb(task_id: int):
    """Get KB entries related to a task (via task, story, or epic)"""
    # Get task info first
    tasks = get_odoo_client().execute(
        "amp.task",
        "read",
        [[task_id]],
        {"fields": ["story_id", "epic_id", "project_id"]},
    )
    if not tasks:
        raise HTTPException(status_code=404, detail="Task not found")

    task = tasks[0]
    domain = [
        "|",
        "|",
        "|",
        ["task_id", "=", task_id],
        ["story_id", "=", task.get("story_id")[0] if task.get("story_id") else False],
        ["epic_id", "=", task.get("epic_id")[0] if task.get("epic_id") else False],
        ["project_id", "=", task["project_id"][0]],
    ]

    entries = get_odoo_client().execute(
        "amp.knowledge.entry",
        "search_read",
        [domain],
        {
            "fields": [
                "title",
                "entry_type",
                "content_text",
                "created_by_agent",
                "create_date",
            ],
            "limit": 20,
            "order": "create_date desc",
        },
    )
    return {"entries": entries}


# ============== Project Context File (.amp.json) ==============


@app.get("/context/file")
def read_amp_context():
    """Read .amp.json from current directory (via agent)"""
    # This is handled client-side by the agent reading the file
    # MCP just provides the schema definition
    return {
        "schema": {
            "version": "1.0.0",
            "project_id": "integer - Odoo project ID",
            "project_code": "string - Project code/directory name",
            "project_name": "string - Project name",
            "created_at": "ISO timestamp",
            "last_session": "ISO timestamp",
            "current_epic_id": "integer - Currently active epic",
            "current_story_id": "integer - Currently active story",
            "active_task_ids": "array of integers - Tasks being worked on",
            "kb_entry_ids": "array of integers - Related KB entries",
            "context": "object - Custom context data",
        }
    }


@app.post("/context/validate")
def validate_amp_context(context: Dict[str, Any]):
    """Validate .amp.json context"""
    required_fields = ["project_id", "project_code"]
    missing = [f for f in required_fields if f not in context]

    if missing:
        return {"valid": False, "missing_fields": missing}

    # Verify project exists
    try:
        project = get_odoo_client().execute(
            "amp.project", "read", [[context["project_id"]]], {"fields": ["name"]}
        )
        if not project:
            return {"valid": False, "error": "Project not found in Odoo"}
    except Exception as e:
        return {"valid": False, "error": str(e)}

    return {"valid": True, "project_name": project[0]["name"]}


if __name__ == "__main__":
    import uvicorn

    port = int(os.getenv("MCP_PORT", "8000"))
    uvicorn.run(app, host="0.0.0.0", port=port)
