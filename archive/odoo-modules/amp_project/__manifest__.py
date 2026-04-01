{
    "name": "AMP Project",
    "version": "1.1.0",
    "category": "AMP",
    "summary": "Agentic Management Platform — live board, DAG tasks, real-time agent monitoring",
    "description": """
        AMP Project Management:
        - Epic → Story → Task hierarchy with DAG dependencies
        - Live Board: JIRA-style kanban with real-time updates via bus.bus
        - Agent dispatch with rich context written to tickets
        - Knowledge Base integration
    """,
    "author": "AMP",
    "depends": ["base", "mail", "web", "bus"],
    "data": [
        "security/ir.model.access.csv",
        "security/security_rules.xml",
        "data/amp_stages.xml",
        "data/amp_sequence.xml",
        "views/assets.xml",
        "views/amp_project_views.xml",
        "views/amp_epic_views.xml",
        "views/amp_story_views.xml",
        "views/amp_task_views.xml",
        "views/amp_menus.xml",
    ],
    "assets": {
        "web.assets_backend": [
            "amp_project/static/src/css/amp_project.css",
            "amp_project/static/src/css/amp_board.css",
            "amp_project/static/src/css/amp_navigation.css",
            "amp_project/static/src/css/markdown_widget.css",
            "amp_project/static/src/xml/amp_board.xml",
            "amp_project/static/src/xml/markdown_widget.xml",
            "amp_project/static/src/js/amp_board.js",
            "amp_project/static/src/js/markdown_widget.js",
        ],
    },
    "installable": True,
    "application": True,
    "auto_install": False,
    "license": "LGPL-3",
}
