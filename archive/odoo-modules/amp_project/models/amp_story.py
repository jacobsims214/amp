# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpStory(models.Model):
    """Story — a user-facing outcome made up of tasks.

    State machine (manager-controlled, with auto-progression from task completions):
      backlog → in_progress → completed
                            → blocked

    Transitions:
      backlog     → in_progress  when first task is dispatched (auto) or manager calls action_start
      in_progress → completed    when all tasks complete (auto) or manager calls action_complete
      in_progress → blocked      when manager calls action_block
      blocked     → in_progress  when manager calls action_unblock
      any         → backlog      when manager calls action_reset (reopen)
    """

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

    # Story-level dependencies (for epic DAG)
    dependency_ids = fields.Many2many(
        "amp.story",
        "amp_story_dependency_rel",
        "story_id",
        "dependency_story_id",
        string="Depends On",
        help="Stories that must complete before this story can start",
    )
    blocked_by_ids = fields.Many2many(
        "amp.story",
        "amp_story_dependency_rel",
        "dependency_story_id",
        "story_id",
        string="Blocks",
        help="Stories blocked by this story",
    )

    # State — kept intentionally simple
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

    # Statistics (computed from tasks)
    task_count = fields.Integer(string="Tasks", compute="_compute_counts", store=True)
    completed_task_count = fields.Integer(
        string="Completed Tasks", compute="_compute_counts", store=True
    )
    blocked_task_count = fields.Integer(
        string="Blocked Tasks", compute="_compute_counts", store=True
    )
    progress_percentage = fields.Float(
        string="Progress %", compute="_compute_progress", store=True
    )

    @api.depends("task_ids", "task_ids.state")
    def _compute_counts(self):
        for story in self:
            story.task_count = len(story.task_ids)
            story.completed_task_count = len(
                story.task_ids.filtered(lambda t: t.state == "completed")
            )
            story.blocked_task_count = len(
                story.task_ids.filtered(lambda t: t.state == "blocked")
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

    # ── Navigation actions ───────────────────────────────────────────────────────

    def action_view_tasks(self):
        return {
            "name": f"Tasks — {self.name}",
            "type": "ir.actions.act_window",
            "res_model": "amp.task",
            "view_mode": "list,form",
            "domain": [("story_id", "=", self.id)],
            "context": {
                "default_story_id": self.id,
                "default_epic_id": self.epic_id.id if self.epic_id else False,
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
                "|",
                ("story_id", "=", self.id),
                ("epic_id", "=", self.epic_id.id if self.epic_id else False),
                ("project_id", "=", self.project_id.id),
            ],
            "context": {
                "default_project_id": self.project_id.id,
                "default_story_id": self.id,
            },
        }

    # ── Manager-controlled transitions ──────────────────────────────────────────

    def action_start(self):
        """Manager explicitly starts a story."""
        self.write({"state": "in_progress"})
        self.message_post(body="Story started.", message_type="notification")
        self._auto_start_epic()
        return True

    def action_complete(self):
        """Manager explicitly completes a story (overrides auto-check)."""
        self.write({"state": "completed"})
        self.message_post(body="Story completed ✓", message_type="notification")
        self._try_complete_epic()
        return True

    def action_block(self, reason=""):
        """Manager blocks a story."""
        self.write({"state": "blocked"})
        if reason:
            self.message_post(body=f"Blocked: {reason}", message_type="comment")
        return True

    def action_unblock(self):
        """Manager unblocks a story — returns to in_progress."""
        self.write({"state": "in_progress"})
        self.message_post(body="Story unblocked.", message_type="notification")
        return True

    def action_reset(self):
        """Manager resets a story to backlog."""
        self.write({"state": "backlog"})
        return True

    # ── Auto-progression (called by task actions) ────────────────────────────────

    def _action_complete_auto(self):
        """Called automatically when all tasks in this story complete."""
        self.write({"state": "completed"})
        self.message_post(
            body=f"Story completed automatically — all {self.task_count} tasks done.",
            message_type="notification",
        )
        self._try_complete_epic()

    def _auto_start_epic(self):
        """Move parent epic to in_progress if it hasn't started yet."""
        if self.epic_id and self.epic_id.state == "backlog":
            self.epic_id.write({"state": "in_progress"})
            self.epic_id.message_post(
                body="Epic started — first story is in progress.",
                message_type="notification",
            )

    def _try_complete_epic(self):
        """Auto-complete the parent epic when all its stories are done."""
        if not self.epic_id:
            return
        epic = self.epic_id
        all_stories = epic.story_ids
        if not all_stories:
            return
        if all(s.state == "completed" for s in all_stories):
            epic._action_complete_auto()
