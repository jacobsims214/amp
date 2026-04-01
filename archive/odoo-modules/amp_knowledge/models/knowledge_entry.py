# -*- coding: utf-8 -*-
from odoo import models, fields, api
from odoo.tools import html2plaintext


class AmpKnowledgeEntry(models.Model):
    """Knowledge Base Entry — cross-cutting knowledge linked to AMP work items.

    Used by agents to store reusable context: decisions, patterns, gotchas,
    how-tos, and findings that transcend individual tickets.
    """

    _name = "amp.knowledge.entry"
    _description = "AMP Knowledge Entry"
    _inherit = ["mail.thread", "mail.activity.mixin"]
    _order = "create_date desc"
    _rec_name = "title"

    title = fields.Char(string="Title", required=True, index=True, tracking=True)
    content = fields.Html(
        string="Content",
        sanitize=True,
        help="The knowledge itself — patterns, decisions, findings, gotchas.",
    )
    content_text = fields.Text(
        string="Plain Text",
        compute="_compute_content_text",
        store=True,
    )
    entry_type = fields.Selection(
        [
            ("decision", "Decision"),
            ("finding", "Finding"),
            ("howto", "How-To"),
            ("context", "Context"),
            ("reference", "Reference"),
            ("issue", "Issue/Solution"),
        ],
        string="Type",
        default="context",
        index=True,
        tracking=True,
    )
    tags = fields.Char(
        string="Tags",
        help="Comma-separated tags for search (e.g. 'odoo19, manifest, assets')",
        index=True,
    )

    # Links to work items — all optional, link to the most specific relevant item
    project_id = fields.Many2one(
        "amp.project", string="Project", index=True, ondelete="cascade"
    )
    epic_id = fields.Many2one(
        "amp.epic", string="Epic", index=True, ondelete="set null"
    )
    story_id = fields.Many2one(
        "amp.story", string="Story", index=True, ondelete="set null"
    )
    task_id = fields.Many2one(
        "amp.task", string="Task", index=True, ondelete="set null"
    )

    # Agent provenance
    created_by_agent = fields.Char(string="Created By Agent", index=True)

    @api.depends("content")
    def _compute_content_text(self):
        for entry in self:
            entry.content_text = html2plaintext(entry.content) if entry.content else ""

    def action_view_related(self):
        """Open the most specific linked work item."""
        self.ensure_one()
        for model, field in [
            ("amp.task", self.task_id),
            ("amp.story", self.story_id),
            ("amp.epic", self.epic_id),
            ("amp.project", self.project_id),
        ]:
            if field:
                return {
                    "type": "ir.actions.act_window",
                    "res_model": model,
                    "res_id": field.id,
                    "view_mode": "form",
                    "target": "current",
                }
        return False

    def get_related_entries(self, limit=5):
        """Find related knowledge entries based on tags, project, and content similarity."""
        self.ensure_one()
        domain = [("id", "!=", self.id)]

        # Same project entries get higher priority
        if self.project_id:
            domain.append(("project_id", "=", self.project_id.id))

        related_entries = []

        # 1. Entries with matching tags (highest priority)
        if self.tags:
            tag_list = [tag.strip() for tag in self.tags.split(",") if tag.strip()]
            for tag in tag_list:
                tag_domain = domain + [("tags", "ilike", tag)]
                entries = self.search(tag_domain, limit=limit)
                for entry in entries:
                    if entry.id not in [e.id for e in related_entries]:
                        related_entries.append(entry)

        # 2. Same epic/story entries
        if self.epic_id:
            epic_domain = domain + [("epic_id", "=", self.epic_id.id)]
            entries = self.search(epic_domain, limit=limit)
            for entry in entries:
                if entry.id not in [e.id for e in related_entries]:
                    related_entries.append(entry)

        if self.story_id:
            story_domain = domain + [("story_id", "=", self.story_id.id)]
            entries = self.search(story_domain, limit=limit)
            for entry in entries:
                if entry.id not in [e.id for e in related_entries]:
                    related_entries.append(entry)

        # 3. Same entry type in project
        type_domain = domain + [("entry_type", "=", self.entry_type)]
        entries = self.search(type_domain, limit=limit)
        for entry in entries:
            if entry.id not in [e.id for e in related_entries]:
                related_entries.append(entry)

        return related_entries[:limit]

    def action_view_related_entries(self):
        """Open a view showing related knowledge entries."""
        self.ensure_one()
        related = self.get_related_entries(limit=20)

        return {
            "name": f"Related to: {self.title}",
            "type": "ir.actions.act_window",
            "res_model": "amp.knowledge.entry",
            "view_mode": "kanban,list,form",
            "domain": [("id", "in", related.ids)],
            "context": {"search_default_by_type": 1},
        }
