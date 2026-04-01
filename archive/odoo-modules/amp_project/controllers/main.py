# -*- coding: utf-8 -*-
from odoo import http, fields as odoo_fields
from odoo.http import request
from datetime import datetime, timedelta


class AmpController(http.Controller):
    # ── Board data ─────────────────────────────────────────────────────────────

    @http.route("/amp/board/projects", type="jsonrpc", auth="user")
    def board_projects(self, **kwargs):
        """Return all active projects for the project selector."""
        projects = request.env["amp.project"].search(
            [("state", "in", ["draft", "active"])], order="name asc"
        )
        return [
            {
                "id": p.id,
                "name": p.name,
                "code": p.code or "",
                "state": p.state,
                "task_count": p.task_count,
                "completed_task_count": p.completed_task_count,
                "progress_percentage": round(p.progress_percentage, 1),
                "active_agent_count": p.active_agent_count,
            }
            for p in projects
        ]

    @http.route("/amp/board/<int:project_id>/data", type="jsonrpc", auth="user")
    def board_data(self, project_id, **kwargs):
        """Return full board data for a project: epics → stories → tasks."""
        project = request.env["amp.project"].browse(project_id)
        if not project.exists():
            return {"error": "Project not found"}

        epics_data = []
        for epic in project.epic_ids.sorted("sequence"):
            stories_data = []
            for story in epic.story_ids.sorted("sequence"):
                tasks_data = []
                for task in story.task_ids.sorted("sequence, priority desc"):
                    tasks_data.append(_task_dict(task))
                stories_data.append(
                    {
                        "id": story.id,
                        "name": story.name,
                        "state": story.state,
                        "progress_percentage": round(story.progress_percentage, 1),
                        "task_count": story.task_count,
                        "priority": story.priority,
                        "tasks": tasks_data,
                    }
                )
            epics_data.append(
                {
                    "id": epic.id,
                    "name": epic.name,
                    "state": epic.state,
                    "priority": epic.priority,
                    "progress_percentage": round(epic.progress_percentage, 1),
                    "story_count": epic.story_count,
                    "task_count": epic.task_count,
                    "stories": stories_data,
                }
            )

        return {
            "project": {
                "id": project.id,
                "name": project.name,
                "code": project.code or "",
                "state": project.state,
                "progress_percentage": round(project.progress_percentage, 1),
                "task_count": project.task_count,
                "completed_task_count": project.completed_task_count,
                "blocked_task_count": project.blocked_task_count,
                "active_agent_count": project.active_agent_count,
            },
            "epics": epics_data,
        }

    @http.route(
        "/amp/board/<int:project_id>/task/<int:task_id>", type="jsonrpc", auth="user"
    )
    def board_task(self, project_id, task_id, **kwargs):
        """Return a single task's current data (used after a bus update)."""
        task = request.env["amp.task"].browse(task_id)
        if not task.exists() or task.project_id.id != project_id:
            return {"error": "Task not found"}
        return _task_dict(task)

    # ── Legacy realtime endpoint (kept for compatibility) ──────────────────────

    @http.route("/amp/project/<int:project_id>/realtime", type="jsonrpc", auth="user")
    def get_realtime_data(self, project_id, **kwargs):
        project = request.env["amp.project"].browse(project_id)
        if not project.exists():
            return {"error": "Project not found"}
        tasks = project.mapped("epic_ids.story_ids.task_ids")
        cutoff = datetime.now() - timedelta(minutes=30)
        recent = tasks.filtered(lambda t: t.dispatch_time and t.dispatch_time > cutoff)
        return {
            "project_id": project_id,
            "timestamp": datetime.now().isoformat(),
            "stats": {
                "epic_count": project.epic_count,
                "story_count": project.story_count,
                "task_count": project.task_count,
                "completed_count": len(
                    tasks.filtered(lambda t: t.state == "completed")
                ),
                "blocked_count": len(tasks.filtered(lambda t: t.state == "blocked")),
                "in_progress_count": len(
                    tasks.filtered(lambda t: t.state == "in_progress")
                ),
                "progress": project.progress_percentage,
            },
            "agents": list(set(tasks.mapped("agent_id")) - {False}),
            "recent_activity": [
                {
                    "task_name": t.name,
                    "state": t.state,
                    "agent": t.agent_id,
                    "time": t.dispatch_time.isoformat() if t.dispatch_time else None,
                }
                for t in recent[:10]
            ],
        }

    @http.route("/amp/project/<int:project_id>/knowledge", type="jsonrpc", auth="user")
    def get_knowledge_entries(self, project_id, limit=10, **kwargs):
        entries = request.env["amp.knowledge.entry"].search(
            [("project_id", "=", project_id)], limit=limit, order="create_date desc"
        )
        return {
            "entries": [
                {
                    "id": e.id,
                    "title": e.title,
                    "type": e.entry_type,
                    "agent": e.created_by_agent,
                    "date": e.create_date.isoformat(),
                    "epic": e.epic_id.name if e.epic_id else None,
                    "story": e.story_id.name if e.story_id else None,
                }
                for e in entries
            ]
        }

    @http.route("/amp/task/<int:task_id>/kb", type="jsonrpc", auth="user")
    def get_task_knowledge(self, task_id, **kwargs):
        task = request.env["amp.task"].browse(task_id)
        if not task.exists():
            return {"error": "Task not found"}
        domain = [
            "|",
            "|",
            "|",
            ("task_id", "=", task_id),
            ("story_id", "=", task.story_id.id if task.story_id else False),
            ("epic_id", "=", task.epic_id.id if task.epic_id else False),
            ("project_id", "=", task.project_id.id),
        ]
        entries = request.env["amp.knowledge.entry"].search(
            domain, limit=20, order="create_date desc"
        )
        return {
            "task_id": task_id,
            "entries": [
                {
                    "id": e.id,
                    "title": e.title,
                    "type": e.entry_type,
                    "agent": e.created_by_agent,
                    "date": e.create_date.isoformat(),
                }
                for e in entries
            ],
        }


def _task_dict(task):
    """Serialize a task record to a plain dict for the board."""
    return {
        "id": task.id,
        "name": task.name,
        "state": task.state,
        "priority": task.priority,
        "agent_id": task.agent_id or "",
        "agent_session": task.agent_session or "",
        "dag_level": task.dag_level,
        "dag_critical_path": task.dag_critical_path,
        "planned_hours": task.planned_hours,
        "dependency_count": task.dependency_count,
        "blocked_count": task.blocked_count,
        "story_id": task.story_id.id if task.story_id else False,
        "story_name": task.story_id.name if task.story_id else "",
        "epic_id": task.epic_id.id if task.epic_id else False,
        "epic_name": task.epic_id.name if task.epic_id else "",
        "dispatch_time": task.dispatch_time.isoformat() if task.dispatch_time else None,
        "completion_time": task.completion_time.isoformat()
        if task.completion_time
        else None,
        "description_text": (task.description_text or "")[:200],
        "acceptance_criteria": task.acceptance_criteria or "",
    }
