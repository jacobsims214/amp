# -*- coding: utf-8 -*-
from odoo import models, fields, api
from odoo.tools import html2plaintext
import json


class AmpTask(models.Model):
    """Task — the atomic unit of work executed by an agent.

    State machine:
      backlog     → the task is workable (no incomplete deps)
      in_progress → an agent has been dispatched and is working it
      review      → agent submitted for review (optional step)
      completed   → done
      blocked     → has incomplete dependencies (set/cleared automatically)

    The blocked state is FULLY AUTOMATIC:
      - Task created with dependency_ids → state set to blocked
      - All deps complete → state automatically returns to backlog
      - Agents never set blocked. Managers never set blocked manually.
      - Blocking is determined by the dependency graph, not by human decisions.

    Dispatching a blocked task raises a structured error listing the blocking tasks.
    """

    _name = "amp.task"
    _description = "AMP Task"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "dag_level, priority desc, sequence, id"

    name = fields.Char(string="Task Name", required=True, index=True, tracking=True)

    # Instructions for the worker agent
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

    # DAG dependencies
    dependency_ids = fields.Many2many(
        "amp.task",
        "amp_task_dependency_rel",
        "task_id",
        "dependency_task_id",
        string="Depends On",
        domain="[('project_id', '=', project_id), ('id', '!=', id)]",
        help="Tasks that must complete before this task can run",
    )
    blocked_ids = fields.Many2many(
        "amp.task",
        "amp_task_dependency_rel",
        "dependency_task_id",
        "task_id",
        string="Blocks",
        help="Tasks that are waiting on this task",
    )

    # DAG metadata
    dag_level = fields.Integer(
        string="DAG Level",
        default=0,
        help="0 = no deps (runs first). 1 = depends on level-0, etc.",
    )
    dag_critical_path = fields.Boolean(string="Critical Path", default=False)

    # State — blocked is set/cleared by the DAG engine, never by agents
    state = fields.Selection(
        [
            ("backlog", "Backlog"),
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
        [("0", "Low"), ("1", "Medium"), ("2", "High"), ("3", "Critical")],
        string="Priority",
        default="1",
    )
    planned_hours = fields.Float(string="Planned Hours")

    # Agent assignment
    agent_id = fields.Char(string="Assigned Agent", index=True)
    agent_session = fields.Char(string="Agent Session")
    dispatch_time = fields.Datetime(string="Dispatched At")
    completion_time = fields.Datetime(string="Completed At")

    # Context written by manager at dispatch — source of truth for the worker
    context_data = fields.Text(string="Context Data")
    acceptance_criteria = fields.Text(string="Acceptance Criteria")

    # Computed stats
    dependency_count = fields.Integer(
        string="Dependencies",
        compute="_compute_dep_stats",
        store=True,
    )
    blocked_count = fields.Integer(
        string="Blocks",
        compute="_compute_dep_stats",
        store=True,
    )

    # ── Computed fields ──────────────────────────────────────────────────────────

    @api.depends("description")
    def _compute_description_text(self):
        for task in self:
            task.description_text = (
                html2plaintext(task.description) if task.description else ""
            )

    @api.depends("dependency_ids", "blocked_ids")
    def _compute_dep_stats(self):
        for task in self:
            task.dependency_count = len(task.dependency_ids)
            task.blocked_count = len(task.blocked_ids)

    # ── DAG state engine ─────────────────────────────────────────────────────────

    def _recompute_dag_level(self):
        """Set dag_level from the dependency graph."""
        for task in self:
            if not task.dependency_ids:
                task.dag_level = 0
            else:
                task.dag_level = max(dep.dag_level for dep in task.dependency_ids) + 1

    @api.model_create_multi
    def create(self, vals_list):
        tasks = super().create(vals_list)
        # Any task created with dependencies starts blocked
        for task in tasks:
            if task.dependency_ids:
                incomplete = task.dependency_ids.filtered(
                    lambda d: d.state != "completed"
                )
                if incomplete:
                    task.write({"state": "blocked"})
                    task.message_post(
                        body=(
                            f"Task starts blocked — waiting on "
                            f"{len(incomplete)} dependencies: "
                            + ", ".join(f"#{d.id} {d.name}" for d in incomplete)
                        ),
                        message_type="notification",
                    )
            task._recompute_dag_level()
        return tasks

    def write(self, vals):
        old_states = {t.id: t.state for t in self}
        result = super().write(vals)
        if "state" in vals:
            for task in self:
                if old_states.get(task.id) != task.state:
                    task._notify_board("task_state_changed")
        # If dependency_ids changed, re-evaluate blocked state
        if "dependency_ids" in vals:
            for task in self:
                task._recompute_dag_level()
                task._evaluate_blocked_state()
        return result

    def _evaluate_blocked_state(self):
        """Set state=blocked if task has incomplete deps; unblock to backlog if all done."""
        for task in self:
            if task.state in ("in_progress", "completed", "review"):
                continue  # don't touch active/done tasks
            incomplete = task.dependency_ids.filtered(lambda d: d.state != "completed")
            if incomplete and task.state != "blocked":
                task.write({"state": "blocked"})
                task._notify_board("task_blocked")
            elif not incomplete and task.state == "blocked":
                task.write({"state": "backlog"})
                task.message_post(
                    body="All dependencies completed — task is now workable.",
                    message_type="notification",
                )
                task._notify_board("task_unblocked")

    # ── Bus notifications ────────────────────────────────────────────────────────

    def _notify_board(self, event_type="task_update"):
        for task in self:
            if not task.project_id.id:
                continue
            self.env["bus.bus"]._sendone(
                f"amp_board_{task.project_id.id}",
                "amp_task_update",
                {
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
                    "dag_level": task.dag_level,
                    "dag_critical_path": task.dag_critical_path,
                },
            )

    # ── Action methods ───────────────────────────────────────────────────────────

    def action_dispatch(self, agent_id):
        """Dispatch to agent. Raises a structured error if the task is blocked."""
        self.ensure_one()

        if self.state == "blocked":
            blocking = self.dependency_ids.filtered(lambda d: d.state != "completed")
            details = ", ".join(f"#{d.id} '{d.name}' ({d.state})" for d in blocking)
            raise ValueError(
                f"Task #{self.id} '{self.name}' is blocked. "
                f"Complete these dependencies first: {details}"
            )

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
            "story_acceptance_criteria": self.story_id.acceptance_criteria
            if self.story_id
            else None,
            "epic_description": self.epic_id.description if self.epic_id else None,
            "priority": self.priority,
            "dag_level": self.dag_level,
            "dag_critical_path": self.dag_critical_path,
            "dependencies": [
                {"id": d.id, "name": d.name, "state": d.state}
                for d in self.dependency_ids
            ],
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
        self._auto_start_story()
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
        # Unblock any downstream tasks whose deps are now all done
        for downstream in self.blocked_ids:
            downstream._evaluate_blocked_state()
        self._auto_progress_story()
        return True

    def action_mark_review(self):
        self.write({"state": "review"})
        self.message_post(body="Submitted for review.", message_type="notification")
        self._notify_board("task_review")
        return True

    def action_block(self, reason=""):
        """Used ONLY by the Odoo UI — not by agents.
        Agents should just post a comment and let the manager handle it."""
        self.write({"state": "blocked"})
        self.message_post(
            body=f"Manually blocked: {reason}" if reason else "Manually blocked.",
            message_type="comment",
        )
        self._notify_board("task_blocked")
        return True

    def action_return_to_backlog(self):
        self.write({"state": "backlog", "agent_id": False, "dispatch_time": False})
        self._notify_board("task_returned")
        return True

    # ── Story auto-progression ───────────────────────────────────────────────────

    def _auto_start_story(self):
        if self.story_id and self.story_id.state == "backlog":
            self.story_id.write({"state": "in_progress"})
            self.story_id.message_post(
                body="Story started — first task dispatched.",
                message_type="notification",
            )
            self.story_id._auto_start_epic()

    def _auto_progress_story(self):
        if not self.story_id:
            return
        story = self.story_id
        if story.task_ids and all(t.state == "completed" for t in story.task_ids):
            story._action_complete_auto()

    # ── Navigation ───────────────────────────────────────────────────────────────

    def action_view_knowledge(self):
        return {
            "name": f"Knowledge — {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.knowledge.entry",
            "view_mode": "list,form",
            "domain": [
                "|",
                "|",
                "|",
                ("task_id", "=", self.id),
                ("story_id", "=", self.story_id.id if self.story_id else False),
                ("epic_id", "=", self.epic_id.id if self.epic_id else False),
                ("project_id", "=", self.project_id.id),
            ],
            "context": {
                "default_project_id": self.project_id.id,
                "default_task_id": self.id,
            },
        }
