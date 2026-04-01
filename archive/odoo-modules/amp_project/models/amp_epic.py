# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpEpic(models.Model):
    """Epic — a large body of work made up of stories.

    State machine (manager-controlled, with auto-progression from story completions):
      backlog → in_progress → completed
                            → blocked

    Transitions:
      backlog     → in_progress  when first story starts (auto) or manager calls action_start
      in_progress → completed    when all stories complete (auto) or manager calls action_complete
      in_progress → blocked      when manager calls action_block
      blocked     → in_progress  when manager calls action_unblock
    """

    _name = "amp.epic"
    _description = "AMP Epic"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "sequence, id"

    name = fields.Char(string="Epic Name", required=True, index=True, tracking=True)
    description = fields.Text(string="Description")

    # Relations
    project_id = fields.Many2one(
        "amp.project", string="Project", required=True, index=True, ondelete="cascade"
    )
    story_ids = fields.One2many("amp.story", "epic_id", string="Stories")

    # State — aligned with story and task states
    state = fields.Selection(
        [
            ("backlog", "Backlog"),
            ("in_progress", "In Progress"),
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

    # Statistics (computed from stories/tasks)
    story_count = fields.Integer(
        string="Stories", compute="_compute_counts", store=True
    )
    task_count = fields.Integer(string="Tasks", compute="_compute_counts", store=True)
    completed_task_count = fields.Integer(
        string="Completed Tasks", compute="_compute_counts", store=True
    )
    completed_story_count = fields.Integer(
        string="Completed Stories", compute="_compute_counts", store=True
    )
    progress_percentage = fields.Float(
        string="Progress %", compute="_compute_progress", store=True
    )

    @api.depends(
        "story_ids", "story_ids.state", "story_ids.task_ids", "story_ids.task_ids.state"
    )
    def _compute_counts(self):
        for epic in self:
            epic.story_count = len(epic.story_ids)
            epic.completed_story_count = len(
                epic.story_ids.filtered(lambda s: s.state == "completed")
            )
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

    # ── Navigation actions ───────────────────────────────────────────────────────

    def action_view_stories(self):
        return {
            "name": f"Stories — {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.story",
            "view_mode": "list,form",
            "domain": [("epic_id", "=", self.id)],
            "context": {
                "default_epic_id": self.id,
                "default_project_id": self.project_id.id,
            },
        }

    def action_view_knowledge(self):
        return {
            "name": f"Knowledge — {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.knowledge.entry",
            "view_mode": "list,form",
            "domain": [
                "|",
                ("epic_id", "=", self.id),
                ("project_id", "=", self.project_id.id),
            ],
            "context": {
                "default_project_id": self.project_id.id,
                "default_epic_id": self.id,
            },
        }

    # ── Manager-controlled transitions ──────────────────────────────────────────

    def action_start(self):
        """Manager explicitly starts an epic."""
        self.write({"state": "in_progress"})
        self.message_post(body="Epic started.", message_type="notification")
        return True

    def action_complete(self):
        """Manager explicitly completes an epic."""
        self.write({"state": "completed"})
        self.message_post(body="Epic completed ✓", message_type="notification")
        # Ensure all stories are also marked complete
        incomplete = self.story_ids.filtered(lambda s: s.state != "completed")
        if incomplete:
            incomplete.write({"state": "completed"})
        return True

    def action_block(self, reason=""):
        """Manager blocks an epic."""
        self.write({"state": "blocked"})
        if reason:
            self.message_post(body=f"Blocked: {reason}", message_type="comment")
        return True

    def action_unblock(self):
        """Manager unblocks an epic — returns to in_progress."""
        self.write({"state": "in_progress"})
        self.message_post(body="Epic unblocked.", message_type="notification")
        return True

    # ── Auto-progression (called by story actions) ───────────────────────────────

    def _action_complete_auto(self):
        """Called automatically when all stories in this epic complete."""
        self.write({"state": "completed"})
        self.message_post(
            body=f"Epic completed automatically — all {self.story_count} stories done.",
            message_type="notification",
        )
