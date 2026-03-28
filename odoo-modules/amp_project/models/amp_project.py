# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpProject(models.Model):
    """Root project container for AMP"""

    _name = "amp.project"
    _description = "AMP Project"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "create_date desc"

    name = fields.Char(string="Project Name", required=True, index=True, tracking=True)
    code = fields.Char(
        string="Project Code", index=True, help="Directory-based matching code"
    )
    description = fields.Text(string="Description")

    # Status
    active = fields.Boolean(string="Active", default=True)
    state = fields.Selection(
        [
            ("draft", "Draft"),
            ("active", "Active"),
            ("archived", "Archived"),
        ],
        string="Status",
        default="draft",
        tracking=True,
    )

    # Relations
    epic_ids = fields.One2many("amp.epic", "project_id", string="Epics")

    # Statistics
    epic_count = fields.Integer(string="Epics", compute="_compute_counts")
    story_count = fields.Integer(string="Stories", compute="_compute_counts")
    task_count = fields.Integer(string="Tasks", compute="_compute_counts")
    completed_task_count = fields.Integer(string="Completed", compute="_compute_counts")
    blocked_task_count = fields.Integer(string="Blocked", compute="_compute_counts")

    # Agent tracking
    last_session = fields.Datetime(string="Last Agent Session")
    active_agent_count = fields.Integer(
        string="Active Agents", compute="_compute_agent_stats"
    )

    # Progress
    progress_percentage = fields.Float(string="Progress %", compute="_compute_progress")

    @api.depends("epic_ids", "epic_ids.story_ids", "epic_ids.story_ids.task_ids")
    def _compute_counts(self):
        for project in self:
            project.epic_count = len(project.epic_ids)
            project.story_count = sum(len(epic.story_ids) for epic in project.epic_ids)
            all_tasks = project.mapped("epic_ids.story_ids.task_ids")
            project.task_count = len(all_tasks)
            project.completed_task_count = len(
                all_tasks.filtered(lambda t: t.state == "completed")
            )
            project.blocked_task_count = len(
                all_tasks.filtered(lambda t: t.state == "blocked")
            )

    @api.depends("epic_ids.story_ids.task_ids.agent_id")
    def _compute_agent_stats(self):
        for project in self:
            agents = project.mapped("epic_ids.story_ids.task_ids.agent_id")
            project.active_agent_count = len(set(agents) - {False})

    @api.depends("task_count", "completed_task_count")
    def _compute_progress(self):
        for project in self:
            if project.task_count > 0:
                project.progress_percentage = (
                    project.completed_task_count / project.task_count
                ) * 100
            else:
                project.progress_percentage = 0

    def action_activate(self):
        self.write({"state": "active"})

    def action_archive(self):
        self.write({"state": "archived"})

    def action_view_epics(self):
        return {
            "name": "Project Epics",
            "type": "ir.actions.act_window",
            "res_model": "amp.epic",
            "view_mode": "kanban,list,form",
            "domain": [("project_id", "=", self.id)],
            "context": {"default_project_id": self.id},
        }

    def action_open_dashboard(self):
        """Open dashboard form for this project"""
        return {
            "name": f"AMP Dashboard - {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.project",
            "view_mode": "form",
            "res_id": self.id,
            "target": "current",
        }

    def action_view_knowledge(self):
        """Open knowledge entries filtered to this project.
        Built at runtime to avoid cross-module XML load-order issues."""
        return {
            "name": f"Knowledge Base - {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.knowledge.entry",
            "view_mode": "list,form",
            "domain": [("project_id", "=", self.id)],
            "context": {"default_project_id": self.id},
        }

    def action_sync_session(self):
        """Record agent session"""
        self.write({"last_session": fields.Datetime.now()})
        self.message_post(
            body="Agent session synchronized", message_type="notification"
        )
