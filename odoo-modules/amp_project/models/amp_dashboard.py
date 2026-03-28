# -*- coding: utf-8 -*-
from odoo import models, fields, api


class AmpDashboard(models.Model):
    """Dashboard for real-time project monitoring"""

    _name = "amp.dashboard"
    _description = "AMP Dashboard"
    _auto = False  # This is a view model

    # Project Overview
    project_id = fields.Many2one("amp.project", string="Project")
    name = fields.Char(related="project_id.name")

    # Real-time stats
    total_epics = fields.Integer(string="Total Epics")
    total_stories = fields.Integer(string="Total Stories")
    total_tasks = fields.Integer(string="Total Tasks")

    # Task breakdown by state
    backlog_count = fields.Integer(string="Backlog")
    ready_count = fields.Integer(string="Ready")
    in_progress_count = fields.Integer(string="In Progress")
    review_count = fields.Integer(string="Review")
    completed_count = fields.Integer(string="Completed")
    blocked_count = fields.Integer(string="Blocked")

    # Progress
    completion_percentage = fields.Float(string="% Complete")

    # Active agents
    active_agents = fields.Integer(string="Active Agents")

    # Recent activity (last hour)
    recent_completions = fields.Integer(string="Recent Completions")
    recent_starts = fields.Integer(string="Recent Starts")

    @api.model
    def get_project_dashboard(self, project_id):
        """Get dashboard data for a project"""
        project = self.env["amp.project"].browse(project_id)
        if not project.exists():
            return {}

        tasks = project.mapped("epic_ids.story_ids.task_ids")
        now = fields.Datetime.now()
        one_hour_ago = now.replace(hour=now.hour - 1)

        return {
            "project": {
                "id": project.id,
                "name": project.name,
                "state": project.state,
            },
            "counts": {
                "epics": project.epic_count,
                "stories": project.story_count,
                "tasks": project.task_count,
            },
            "by_state": {
                "backlog": len(tasks.filtered(lambda t: t.state == "backlog")),
                "ready": len(tasks.filtered(lambda t: t.state == "ready")),
                "in_progress": len(tasks.filtered(lambda t: t.state == "in_progress")),
                "review": len(tasks.filtered(lambda t: t.state == "review")),
                "completed": len(tasks.filtered(lambda t: t.state == "completed")),
                "blocked": len(tasks.filtered(lambda t: t.state == "blocked")),
            },
            "progress": project.progress_percentage,
            "agents": {
                "active": project.active_agent_count,
                "list": list(
                    set(project.mapped("epic_ids.story_ids.task_ids.agent_id"))
                    - {False}
                ),
            },
            "recent": {
                "completions": len(
                    tasks.filtered(
                        lambda t: t.completion_time and t.completion_time > one_hour_ago
                    )
                ),
                "starts": len(
                    tasks.filtered(
                        lambda t: t.dispatch_time and t.dispatch_time > one_hour_ago
                    )
                ),
            },
            "blocked_tasks": [
                {"id": t.id, "name": t.name, "agent": t.agent_id}
                for t in tasks.filtered(lambda t: t.state == "blocked")
            ],
            "in_progress_tasks": [
                {
                    "id": t.id,
                    "name": t.name,
                    "agent": t.agent_id,
                    "started": t.dispatch_time,
                }
                for t in tasks.filtered(lambda t: t.state == "in_progress")
            ],
        }
