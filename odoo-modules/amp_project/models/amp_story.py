# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpStory(models.Model):
    """Story - User-facing functionality"""

    _name = "amp.story"
    _description = "AMP Story"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "sequence, id"

    name = fields.Char(string="Story Name", required=True, index=True, tracking=True)
    description = fields.Text(string="Description")
    acceptance_criteria = fields.Text(string="Acceptance Criteria")

    # Relations
    project_id = fields.Many2one(
        "amp.project", string="Project", required=True, index=True
    )
    epic_id = fields.Many2one("amp.epic", string="Epic", index=True, ondelete="cascade")
    task_ids = fields.One2many("amp.task", "story_id", string="Tasks")

    # Dependencies (DAG)
    dependency_ids = fields.Many2many(
        "amp.story",
        "amp_story_dependency_rel",
        "story_id",
        "dependency_story_id",
        string="Depends On",
        help="Stories that must complete before this story can start",
    )
    blocked_ids = fields.Many2many(
        "amp.story",
        "amp_story_dependency_rel",
        "dependency_story_id",
        "story_id",
        string="Blocks",
        help="Stories blocked by this story",
    )

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

    # Statistics
    task_count = fields.Integer(string="Tasks", compute="_compute_counts")
    completed_task_count = fields.Integer(string="Completed", compute="_compute_counts")
    progress_percentage = fields.Float(string="Progress %", compute="_compute_progress")
    is_ready = fields.Boolean(string="Ready to Start", compute="_compute_is_ready")

    @api.depends("task_ids")
    def _compute_counts(self):
        for story in self:
            story.task_count = len(story.task_ids)
            story.completed_task_count = len(
                story.task_ids.filtered(lambda t: t.state == "completed")
            )

    @api.depends("task_count", "completed_task_count")
    def _compute_progress(self):
        for story in self:
            if story.task_count > 0:
                story.progress_percentage = (
                    story.completed_task_count / story.task_count
                ) * 100
            else:
                story.progress_percentage = 0

    @api.depends("dependency_ids", "dependency_ids.state")
    def _compute_is_ready(self):
        for story in self:
            if not story.dependency_ids:
                story.is_ready = True
            else:
                story.is_ready = all(
                    dep.state == "completed" for dep in story.dependency_ids
                )

    def action_view_tasks(self):
        return {
            "name": f"Story Tasks - {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.task",
            "view_mode": "kanban,list,form",
            "domain": [("story_id", "=", self.id)],
            "context": {
                "default_story_id": self.id,
                "default_epic_id": self.epic_id.id,
                "default_project_id": self.project_id.id,
            },
        }

    def action_mark_ready(self):
        self.write({"state": "ready"})

    def action_start(self):
        self.write({"state": "in_progress"})

    def action_mark_review(self):
        """Mark story as in review"""
        self.write({"state": "review"})

    def action_complete(self):
        self.write({"state": "completed"})
        # Mark all tasks as completed
        self.task_ids.write({"state": "completed"})

    def action_block(self, reason=""):
        self.write({"state": "blocked"})
        if reason:
            self.message_post(body=f"Blocked: {reason}", message_type="comment")

    def action_unblock(self):
        if self.is_ready:
            self.write({"state": "ready"})
        else:
            self.write({"state": "backlog"})
