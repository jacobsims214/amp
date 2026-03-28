# AMP Odoo Modules - Fix List

You are working in `/Users/jacobsims/development/simstech/amp/odoo-modules`.
Fix all issues below in order. After all fixes are applied, attempt to install
`amp_project` first, then `amp_knowledge` via the Odoo UI or:

```
docker compose run --rm odoo odoo -c /etc/odoo/odoo-19.conf -d odoo19 -i amp_project,amp_knowledge --stop-after-init
```

---

## Fix 1 — amp_project/views/amp_project_views.xml

**Problem A:** The `<button>` in the Knowledge Base notebook page uses a cross-module
external ID reference `%(amp_knowledge.action_amp_knowledge_entry)d`. Because
`amp_project` is loaded BEFORE `amp_knowledge` (amp_knowledge depends on amp_project),
that external ID does not exist yet and Odoo throws:
`ValueError: External ID not found in the system: amp_knowledge.action_amp_knowledge_entry`

**Problem B:** The `attrs=` syntax on buttons uses the old Odoo 16 dict style.
Odoo 17+ uses plain Python expressions in `invisible=`.

**Fix:** Replace the entire Knowledge Base notebook page — remove the broken
cross-module button and the bad `attrs=` on header buttons. The knowledge button
in the stat box should call a Python method `action_view_knowledge` on the model
(see Fix 6 below) which builds the action at runtime, avoiding load-order issues.

Replace the form view's `<header>` buttons:
```xml
<button name="action_activate" string="Activate" type="object" class="oe_highlight"
        invisible="state != 'draft'"/>
<button name="action_archive" string="Archive" type="object"
        invisible="state == 'archived'"/>
<button name="action_sync_session" string="Sync Session" type="object"/>
<field name="state" widget="statusbar" statusbar_visible="draft,active,archived"/>
```

Replace the `oe_button_box`:
```xml
<div class="oe_button_box" name="button_box">
    <button name="action_view_epics" type="object" class="oe_stat_button" icon="fa-sitemap">
        <field name="epic_count" widget="statinfo" string="Epics"/>
    </button>
    <button name="action_view_knowledge" type="object" class="oe_stat_button" icon="fa-book">
        <span class="o_stat_text">Knowledge</span>
    </button>
</div>
```

Remove the entire `<page string="Knowledge Base">` notebook page entirely.
(Navigation to KB is handled via the stat button and the KB module's own menu.)

---

## Fix 2 — amp_project/views/amp_dashboard_views.xml

**Problem:** `view_mode="dashboard"` is not a valid Odoo view type. Valid types are:
`list, form, graph, pivot, calendar, kanban, search, qweb, cohort, gantt, grid,
hierarchy, map, activity`.
This will cause a crash when any code tries to open the dashboard action.

Also `action_open_dashboard` in `amp_project.py` returns `view_mode: "dashboard"` too.

**Fix:** Replace the dashboard action record to use `form` view mode with the
existing `view_amp_project_dashboard` view ref, and remove the bad view_mode:

```xml
<record id="action_amp_project_dashboard" model="ir.actions.act_window">
    <field name="name">Project Dashboard</field>
    <field name="res_model">amp.project</field>
    <field name="view_mode">form</field>
    <field name="view_id" ref="view_amp_project_dashboard"/>
</record>
```

Also in the dashboard form view itself, remove `edit="false"` and `create="false"`
(deprecated in Odoo 17, use `js_class` or leave defaults).

---

## Fix 3 — amp_project/views/amp_epic_views.xml

**Problem:** Typo — double closing brace `}}` in `attrs` on the Complete button:
```xml
attrs="{'invisible': [('state', '=', 'completed')]}}"/>
```
There is a stray `}` at the end. Same issue exists on the button but since we're
moving to Odoo 17+ `invisible=` syntax anyway, fix all buttons in the form header:

```xml
<button name="action_move_to_planning" string="Start Planning" type="object"
        class="oe_highlight" invisible="state != 'backlog'"/>
<button name="action_start" string="Start Epic" type="object"
        class="oe_highlight" invisible="state not in ('backlog', 'planning')"/>
<button name="action_complete" string="Complete" type="object"
        class="oe_highlight" invisible="state == 'completed'"/>
<field name="state" widget="statusbar" statusbar_visible="backlog,planning,in_progress,completed"/>
```

---

## Fix 4 — amp_project/views/amp_story_views.xml

**Problem:** Same `}}` typo on multiple buttons in story form header. Fix all:

```xml
<button name="action_mark_ready" string="Mark Ready" type="object"
        class="oe_highlight" invisible="state != 'backlog' or not is_ready"/>
<button name="action_start" string="Start" type="object"
        class="oe_highlight" invisible="state not in ('backlog', 'ready')"/>
<button name="action_mark_review" string="Submit for Review" type="object"
        invisible="state != 'in_progress'"/>
<button name="action_complete" string="Complete" type="object"
        class="oe_highlight" invisible="state == 'completed'"/>
<button name="action_block" string="Block" type="object"
        class="btn-warning" invisible="state == 'blocked'"/>
<button name="action_unblock" string="Unblock" type="object"
        invisible="state != 'blocked'"/>
<field name="state" widget="statusbar" statusbar_visible="backlog,ready,in_progress,review,completed"/>
```

---

## Fix 5 — amp_project/security/ir.model.access.csv

**Problem:** The last line references `model_amp_dashboard` which does not exist
as a model. The dashboard is just an `ir.ui.view` form, not a model class.
Odoo will fail to install with an unknown model reference in the ACL CSV.

**Fix:** Remove this line entirely:
```
access_amp_dashboard_user,amp.dashboard user,model_amp_dashboard,base.group_user,1,0,0,0
```

---

## Fix 6 — amp_project/models/amp_project.py

**Problem A:** `action_view_epics` returns `view_mode: "kanban,tree,form"` — `tree` is invalid.

**Problem B:** `action_open_dashboard` returns `view_mode: "dashboard"` — invalid.

**Problem C:** There is no `action_view_knowledge` method but we added a button for it in Fix 1.

**Fix:** Replace the three methods:

```python
def action_view_epics(self):
    return {
        'name': 'Project Epics',
        'type': 'ir.actions.act_window',
        'res_model': 'amp.epic',
        'view_mode': 'kanban,list,form',
        'domain': [('project_id', '=', self.id)],
        'context': {'default_project_id': self.id},
    }

def action_open_dashboard(self):
    """Open dashboard form for this project"""
    return {
        'name': f'AMP Dashboard - {self.name}',
        'type': 'ir.actions.act_window',
        'res_model': 'amp.project',
        'view_mode': 'form',
        'res_id': self.id,
        'target': 'current',
    }

def action_view_knowledge(self):
    """Open knowledge entries filtered to this project.
    Built at runtime to avoid cross-module XML load-order issues."""
    return {
        'name': f'Knowledge Base - {self.name}',
        'type': 'ir.actions.act_window',
        'res_model': 'amp.knowledge.entry',
        'view_mode': 'list,form',
        'domain': [('project_id', '=', self.id)],
        'context': {'default_project_id': self.id},
    }
```

---

## Fix 7 — amp_project/models/amp_epic.py

**Problem:** `action_view_stories` returns `view_mode: "kanban,tree,form"` — `tree` invalid.

**Fix:**
```python
def action_view_stories(self):
    return {
        'name': f'Epic Stories - {self.name}',
        'type': 'ir.actions.act_window',
        'res_model': 'amp.story',
        'view_mode': 'kanban,list,form',
        'domain': [('epic_id', '=', self.id)],
        'context': {
            'default_epic_id': self.id,
            'default_project_id': self.project_id.id,
        },
    }
```

---

## Fix 8 — amp_project/models/amp_story.py

**Problem:** `action_view_tasks` returns `view_mode: "kanban,tree,form"` — `tree` invalid.

**Fix:**
```python
def action_view_tasks(self):
    return {
        'name': f'Story Tasks - {self.name}',
        'type': 'ir.actions.act_window',
        'res_model': 'amp.task',
        'view_mode': 'kanban,list,form',
        'domain': [('story_id', '=', self.id)],
        'context': {
            'default_story_id': self.id,
            'default_epic_id': self.epic_id.id,
            'default_project_id': self.project_id.id,
        },
    }
```

---

## Fix 9 — amp_knowledge/models/knowledge_entity.py

**Problem:** `AmpKnowledgeEntity` references `project.project` and `project.task`
(Odoo's built-in Project module) but `amp_knowledge` does not depend on `project`
in its `__manifest__.py`. It also calls `self.env["amp.knowledge.relationship"]`
which is a model that does not exist anywhere.

This model appears to be a legacy/experimental model that was never finished.
It is NOT referenced in any view or menu. The cleanest fix is to either:

**Option A (recommended):** Delete `knowledge_entity.py` entirely and remove it
from `amp_knowledge/models/__init__.py`.

**Option B:** Fix the references — change `project.project` → `amp.project`,
change `project.task` → `amp.task`, and remove the `create_relationship` method
(or stub it out without calling the nonexistent model).

Go with **Option A**. Steps:
1. Delete `amp_knowledge/models/knowledge_entity.py`
2. Edit `amp_knowledge/models/__init__.py` — remove the import line for `knowledge_entity`

---

## Fix 10 — amp_knowledge/security/ir.model.access.csv

**Problem:** Only has ACL for `amp.knowledge.entry`. The `amp.knowledge.entity`
model (if kept) also needs ACL. Since we are deleting `knowledge_entity.py` in
Fix 9, no change needed here — but verify the file only contains:

```
id,name,model_id:id,group_id:id,perm_read,perm_write,perm_create,perm_unlink
access_amp_knowledge_entry_user,amp.knowledge.entry user,model_amp_knowledge_entry,base.group_user,1,1,1,0
access_amp_knowledge_entry_manager,amp.knowledge.entry manager,model_amp_knowledge_entry,base.group_system,1,1,1,1
```

If `model_amp_knowledge_entity` is still in there, remove those lines.

---

## Fix 11 — amp_project/views/assets.xml (verify)

Open this file and check if it references any JS/CSS files that don't exist on
disk. If `amp_project/static/src/js/amp_dashboard.js` or
`amp_project/static/src/css/amp_project.css` are missing, either create stub
files or remove those lines from `assets.xml` and the `__manifest__.py` assets
dict. Missing static asset references cause silent install failures.

Check with:
```bash
ls amp_project/static/src/js/
ls amp_project/static/src/css/
```

---

## Summary of files to touch

| File | Action |
|------|--------|
| `amp_project/views/amp_project_views.xml` | Remove KB notebook page, fix button attrs |
| `amp_project/views/amp_dashboard_views.xml` | Fix view_mode dashboard → form |
| `amp_project/views/amp_epic_views.xml` | Fix }} typo, update attrs to invisible= |
| `amp_project/views/amp_story_views.xml` | Fix }} typo, update attrs to invisible= |
| `amp_project/security/ir.model.access.csv` | Remove model_amp_dashboard line |
| `amp_project/models/amp_project.py` | Fix view_modes, add action_view_knowledge |
| `amp_project/models/amp_epic.py` | Fix view_mode tree → list |
| `amp_project/models/amp_story.py` | Fix view_mode tree → list |
| `amp_knowledge/models/knowledge_entity.py` | Delete entirely |
| `amp_knowledge/models/__init__.py` | Remove knowledge_entity import |
