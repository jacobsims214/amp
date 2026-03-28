# -*- coding: utf-8 -*-
from odoo import models, fields, api
from odoo.tools import html2plaintext
import json


class AmpTask(models.Model):
    """Task - Specific work item with DAG dependencies"""

    _name = "amp.task"
    _description = "AMP Task"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "sequence, priority desc, id"

    name = fields.Char(string="Task Name", required=True, index=True, tracking=True)

    # This IS the instruction for the worker agent
    description = fields.Html(
        string="Instructions",
        sanitize=True,
        help="Complete instructions for the worker agent",
    )
    description_text = fields.Text(
        string="Plain Text Instructions",
        compute="_compute_description_text",
        store=True,
    )

    # Relations
    project_id = fields.Many2one(
        "amp.project", string="Project", required=True, index=True
    )
    epic_id = fields.Many2one("amp.epic", string="Epic", index=True)
    story_id = fields.Many2one(
        "amp.story", string="Story", index=True, ondelete="cascade"
    )

    # Dependencies (DAG)
    dependency_ids = fields.Many2many(
        "amp.task",
        "amp_task_dependency_rel",
        "task_id",
        "dependency_task_id",
        string="Depends On",
        domain="[('project_id', '=', project_id), ('id', '!=', id)]",
        help="Tasks that must complete before this task can start",
    )
    blocked_ids = fields.Many2many(
        "amp.task",
        "amp_task_dependency_rel",
        "dependency_task_id",
        "task_id",
        string="Blocks",
        help="Tasks blocked by this task",
    )

    # DAG Metadata
    dag_level = fields.Integer(
        string="DAG Level",
        default=0,
        help="Topological sort level (0 = no dependencies)",
    )
    dag_critical_path = fields.Boolean(string="On Critical Path", default=False)

    # Status
    state = fields.Selection(
        [
            ("backlog", "Backlog"),
            ("ready", "Ready"),
            ("in_progress", "In Progress"),
            ("review", "Review"),
            ("completed", "Completed"),
            ("blocked", "Blocked"),
        ],
        string="Status",
        default="backlog",
        tracking=True,
    )

    # Planning
    sequence = fields.Integer(string="Sequence", default=10)
    priority = fields.Selection(
        [
            ("0", "Low"),
            ("1", "Medium"),
            ("2", "High"),
            ("3", "Critical"),
        ],
        string="Priority",
        default="1",
    )
    planned_hours = fields.Float(string="Planned Hours")
    actual_hours = fields.Float(string="Actual Hours", compute="_compute_actual_hours")

    # Agent Assignment
    agent_id = fields.Char(
        string="Assigned Agent",
        index=True,
        help="Agent identifier (e.g., amp-worker-1)",
    )
    agent_session = fields.Char(string="Agent Session")
    dispatch_time = fields.Datetime(string="Dispatched At")
    completion_time = fields.Datetime(string="Completed At")

    # Context
    context_data = fields.Text(
        string="Context Data", help="JSON-serialized context for the agent"
    )
    acceptance_criteria = fields.Text(string="Acceptance Criteria")

    # Computed fields
    is_ready = fields.Boolean(
        string="Ready to Start", compute="_compute_is_ready", store=True
    )
    dependency_count = fields.Integer(
        string="Dependencies", compute="_compute_dependency_stats"
    )
    blocked_count = fields.Integer(
        string="Blocked Tasks", compute="_compute_dependency_stats"
    )

    @api.depends("description")
    def _compute_description_text(self):
        for task in self:
            task.description_text = (
                html2plaintext(task.description) if task.description else ""
            )

    @api.depends("dependency_ids", "dependency_ids.state")
    def _compute_is_ready(self):
        for task in self:
            if not task.dependency_ids:
                task.is_ready = True
                task.dag_level = 0
            else:
                all_complete = all(
                    dep.state == "completed" for dep in task.dependency_ids
                )
                task.is_ready = all_complete
                max_level = (
                    max(dep.dag_level for dep in task.dependency_ids)
                    if task.dependency_ids
                    else -1
                )
                task.dag_level = max_level + 1

    @api.depends("dependency_ids", "blocked_ids")
    def _compute_dependency_stats(self):
        for task in self:
            task.dependency_count = len(task.dependency_ids)
            task.blocked_count = len(task.blocked_ids)

    @api.depends("message_ids")
    def _compute_actual_hours(self):
        for task in self:
            task.actual_hours = 0.0

    # ── Bus notification helpers ────────────────────────────────────────────────

    def _notify_board(self, event_type="task_update"):
        """Push a lightweight update notification onto the Odoo bus so the
        AmpBoard OWL component can refresh without polling."""
        for task in self:
            project_id = task.project_id.id
            if not project_id:
                continue
            channel = f"amp_board_{project_id}"
            payload = {
                "type": event_type,
                "task_id": task.id,
                "task_name": task.name,
                "state": task.state,
                "agent_id": task.agent_id or False,
                "story_id": task.story_id.id if task.story_id else False,
                "story_name": task.story_id.name if task.story_id else False,
                "epic_id": task.epic_id.id if task.epic_id else False,
                "epic_name": task.epic_id.name if task.epic_id else False,
                "priority": task.priority,
                "is_ready": task.is_ready,
                "dag_level": task.dag_level,
                "dag_critical_path": task.dag_critical_path,
                "planned_hours": task.planned_hours,
            }
            self.env["bus.bus"]._sendone(channel, "amp_task_update", payload)

    # ── Action / business methods ────────────────────────────────────────────────

    def action_dispatch(self, agent_id):
        """Dispatch task to agent.  Writes rich context so the sub-agent has
        everything it needs from the ticket alone (no extra lookups required)."""
        self.ensure_one()
        if not self.is_ready:
            raise ValueError("Task is not ready - dependencies not complete")

        # Build rich context block for the worker agent
        context = {
            "task_id": self.id,
            "task_name": self.name,
            "instructions": self.description_text or "",
            "acceptance_criteria": self.acceptance_criteria or "",
            "project_id": self.project_id.id,
            "project_name": self.project_id.name,
            "epic_id": self.epic_id.id if self.epic_id else None,
            "epic_name": self.epic_id.name if self.epic_id else None,
            "story_id": self.story_id.id if self.story_id else None,
            "story_name": self.story_id.name if self.story_id else None,
            "story_description": self.story_id.description if self.story_id else None,
            "story_acceptance_criteria": (
                self.story_id.acceptance_criteria if self.story_id else None
            ),
            "epic_description": self.epic_id.description if self.epic_id else None,
            "priority": self.priority,
            "planned_hours": self.planned_hours,
            "dag_level": self.dag_level,
            "dag_critical_path": self.dag_critical_path,
            "dependencies": [
                {"id": dep.id, "name": dep.name, "state": dep.state}
                for dep in self.dependency_ids
            ],
            "blocked_tasks": [{"id": t.id, "name": t.name} for t in self.blocked_ids],
        }

        self.write(
            {
                "state": "in_progress",
                "agent_id": agent_id,
                "dispatch_time": fields.Datetime.now(),
                "context_data": json.dumps(context),
            }
        )
        self.message_post(
            body=f"Dispatched to agent: <b>{agent_id}</b>",
            message_type="notification",
        )
        self._notify_board("task_dispatched")
        return True

    def action_start(self):
        self.write({"state": "in_progress"})
        self._notify_board("task_started")
        return True

    def action_mark_review(self):
        self.write({"state": "review"})
        self.message_post(body="Task submitted for review", message_type="notification")
        self._notify_board("task_review")
        return True

    def action_complete(self):
        self.write(
            {
                "state": "completed",
                "completion_time": fields.Datetime.now(),
            }
        )
        self.message_post(body="Task completed ✓", message_type="notification")
        self._notify_board("task_completed")
        self._unblock_dependents()
        return True

    def action_block(self, reason=""):
        self.write({"state": "blocked"})
        if reason:
            self.message_post(body=f"Blocked: {reason}", message_type="comment")
        self._notify_board("task_blocked")
        return True

    def action_return_to_backlog(self):
        self.write(
            {
                "state": "backlog",
                "agent_id": False,
                "dispatch_time": False,
            }
        )
        self._notify_board("task_returned")
        return True

    def _unblock_dependents(self):
        """Check if completing this task unblocks any dependents."""
        for dependent in self.blocked_ids:
            if dependent.state == "blocked" and dependent.is_ready:
                dependent.write({"state": "ready"})
                dependent.message_post(
                    body=f"Unblocked — all dependencies complete "
                    f"(triggered by completion of {self.name})",
                    message_type="notification",
                )
                dependent._notify_board("task_unblocked")

    def get_dependency_chain(self):
        self.ensure_one()
        chain = []
        visited = set()
        stack = list(self.dependency_ids)
        while stack:
            task = stack.pop(0)
            if task.id not in visited:
                visited.add(task.id)
                chain.append(task)
                stack.extend(task.dependency_ids)
        return chain

    def get_blocked_chain(self):
        self.ensure_one()
        chain = []
        visited = set()
        stack = list(self.blocked_ids)
        while stack:
            task = stack.pop(0)
            if task.id not in visited:
                visited.add(task.id)
                chain.append(task)
                stack.extend(task.blocked_ids)
        return chain

    def get_task_data_for_agent(self):
        """Return task data dict ready for agent consumption."""
        self.ensure_one()
        return {
            "id": self.id,
            "name": self.name,
            "instructions": self.description_text,
            "acceptance_criteria": self.acceptance_criteria,
            "context": json.loads(self.context_data) if self.context_data else {},
            "dependencies": [dep.name for dep in self.dependency_ids],
            "project": self.project_id.name,
            "epic": self.epic_id.name if self.epic_id else None,
            "story": self.story_id.name if self.story_id else None,
        }
