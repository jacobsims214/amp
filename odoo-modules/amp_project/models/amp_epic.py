# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpEpic(models.Model):
    """Epic - Large body of work containing stories"""

    _name = "amp.epic"
    _description = "AMP Epic"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "sequence, id"

    name = fields.Char(string="Epic Name", required=True, index=True, tracking=True)
    code = fields.Char(string="Epic Code", index=True)
    description = fields.Text(string="Description")

    # Relations
    project_id = fields.Many2one(
        "amp.project", string="Project", required=True, index=True, ondelete="cascade"
    )
    story_ids = fields.One2many("amp.story", "epic_id", string="Stories")

    # Status
    state = fields.Selection(
        [
            ("backlog", "Backlog"),
            ("planning", "Planning"),
            ("in_progress", "In Progress"),
            ("completed", "Completed"),
            ("cancelled", "Cancelled"),
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

    # Statistics
    story_count = fields.Integer(string="Stories", compute="_compute_counts")
    task_count = fields.Integer(string="Tasks", compute="_compute_counts")
    completed_task_count = fields.Integer(string="Completed", compute="_compute_counts")
    progress_percentage = fields.Float(string="Progress %", compute="_compute_progress")

    # DAG Data
    dag_json = fields.Text(string="DAG Data", help="JSON-serialized DAG structure")

    @api.depends("story_ids", "story_ids.task_ids")
    def _compute_counts(self):
        for epic in self:
            epic.story_count = len(epic.story_ids)
            all_tasks = epic.mapped("story_ids.task_ids")
            epic.task_count = len(all_tasks)
            epic.completed_task_count = len(
                all_tasks.filtered(lambda t: t.state == "completed")
            )

    @api.depends("task_count", "completed_task_count")
    def _compute_progress(self):
        for epic in self:
            if epic.task_count > 0:
                epic.progress_percentage = (
                    epic.completed_task_count / epic.task_count
                ) * 100
            else:
                epic.progress_percentage = 0

    def action_view_stories(self):
        return {
            "name": f"Epic Stories - {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.story",
            "view_mode": "kanban,list,form",
            "domain": [("epic_id", "=", self.id)],
            "context": {
                "default_epic_id": self.id,
                "default_project_id": self.project_id.id,
            },
        }

    def action_move_to_planning(self):
        self.write({"state": "planning"})

    def action_start(self):
        self.write({"state": "in_progress"})

    def action_complete(self):
        self.write({"state": "completed"})
        # Mark all stories as completed
        self.story_ids.action_complete()
