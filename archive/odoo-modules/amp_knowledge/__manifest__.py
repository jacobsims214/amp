{
    "name": "AMP Knowledge Base",
    "version": "1.1.0",
    "category": "AMP",
    "summary": "Cross-cutting knowledge storage for AMP agents",
    "description": """
        Stores reusable knowledge linked to AMP work items:
        decisions, findings, how-tos, gotchas, context.
        Written by agents, read by agents and humans.
    """,
    "author": "AMP",
    "depends": ["base", "mail", "amp_project"],
    "data": [
        "security/ir.model.access.csv",
        "views/amp_knowledge_views.xml",
        "views/amp_knowledge_menus.xml",
    ],
    "installable": True,
    "application": False,
    "auto_install": False,
    "license": "LGPL-3",
}
