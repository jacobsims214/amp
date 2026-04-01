---
name: odoo-views
description: Odoo 19 view XML rules validated against RelaxNG schemas — search, list, form, and common element constraints
---

# Odoo 19 View XML Rules

These rules are sourced directly from the Odoo 19 RelaxNG schemas at:
`/usr/lib/python3/dist-packages/odoo/addons/base/rng/`

Always validate a search view before deploying:
```bash
docker exec odoo-odoo-1 python3 -c "
from lxml import etree
rng = etree.RelaxNG(etree.parse('/usr/lib/python3/dist-packages/odoo/addons/base/rng/search_view.rng'))
doc = etree.parse('path/to/views.xml')
search = doc.find('.//search')
print('VALID' if rng.validate(search) else [str(e) for e in rng.error_log])
"
```

---

## Search views (`<search>`)

Valid children of `<search>` (in any order):
- `<field>` — search field
- `<filter>` — domain or group-by filter
- `<separator/>` — visual divider
- `<group>` — groups filters in the dropdown
- `<searchpanel>` — sidebar panel (at most one)
- `<newline/>`

### `<field>` in search views
```xml
<field name="title"
       string="Title / Content / Tags"
       filter_domain="['|', '|', ('title', 'ilike', self), ('content_text', 'ilike', self), ('tags', 'ilike', self)]"/>
<field name="project_id"/>
```
- `filter_domain` — valid, searches across multiple fields; use `self` as placeholder for input value
- `string` — overrides the label shown in the search bar
- Order fields BEFORE filters

### `<filter>` in search views
```xml
<!-- Domain filter -->
<filter string="Decisions" name="decisions" domain="[('entry_type', '=', 'decision')]"/>

<!-- Group-by filter — MUST use empty domain=[], context carries the grouping -->
<filter string="Project" name="by_project" domain="[]" context="{'group_by': 'project_id'}"/>
```

### `<group>` in search views — CRITICAL Odoo 19 rules
```xml
<!-- CORRECT — no attributes except standard ones -->
<group>
    <filter string="Project"  name="by_project" domain="[]" context="{'group_by': 'project_id'}"/>
    <filter string="Type"     name="by_type"    domain="[]" context="{'group_by': 'entry_type'}"/>
</group>

<!-- WRONG — expand and string are NOT valid on group in search views -->
<group expand="0" string="Group By">  <!-- INVALID in Odoo 19 -->
```

The `<group>` element in search views uses the common.rng definition which has NO `expand` attribute.
The `string` attribute is also not in the schema. Remove both.

### `<separator/>` in search views
```xml
<separator/>  <!-- valid between field/filter blocks -->
```

---

## List views (`<list>`)

Odoo 19 renamed `<tree>` to `<list>`. Both still work but `<list>` is preferred.

```xml
<list string="Records" default_order="create_date desc">
    <field name="name"/>
    <field name="state" optional="show"/>    <!-- show/hide column toggle -->
    <field name="project_id" optional="hide"/>
</list>
```

- `decoration-info`, `decoration-warning`, `decoration-success`, `decoration-muted` — valid row/cell decorations
- `optional="show|hide"` — user can toggle column visibility

---

## Form views (`<form>`)

### `invisible` attribute — Odoo 19 syntax
```xml
<!-- CORRECT — Odoo 19 uses Python expressions directly -->
<field name="epic_id" invisible="not project_id"/>
<field name="story_id" invisible="not epic_id"/>
<button invisible="state != 'draft'"/>

<!-- WRONG — old Odoo 16 syntax -->
<field name="epic_id" attrs="{'invisible': [('project_id', '=', False)]}"/>  <!-- INVALID -->
```

### `domain` on relational fields in forms
```xml
<!-- String expression referencing sibling fields — valid -->
<field name="epic_id" domain="[('project_id', '=', project_id)]"/>
```

### Html widget
```xml
<!-- CORRECT -->
<field name="content" widget="html"/>

<!-- options="{'style-inline': true}" is deprecated in Odoo 19 — remove it -->
```

---

## Manifest assets (Odoo 17+)

Do NOT use `<template inherit_id="web.assets_backend">` in XML files.
Use the `assets` key in `__manifest__.py`:

```python
"assets": {
    "web.assets_backend": [
        "my_module/static/src/css/styles.css",
        "my_module/static/src/xml/templates.xml",
        "my_module/static/src/js/component.js",
    ],
},
```

---

## Controller routes (Odoo 19)

```python
# CORRECT — type='jsonrpc' (type='json' is deprecated alias, causes warnings)
@http.route("/my/route", type="jsonrpc", auth="user")

# WRONG — deprecated
@http.route("/my/route", type="json", auth="user")
```

---

## Common mistakes that cause "Invalid view" errors

1. `<group expand="0" string="Group By">` in search view — remove `expand` and `string`
2. `attrs={}` on any element — use direct `invisible=`, `readonly=` Python expressions
3. `options="{'style-inline': true}"` on html widget — remove the options attribute
4. `<tree>` — use `<list>` (though `<tree>` still loads, it's deprecated)
5. `<field>` after `<filter>` in search views — put all fields first, then filters
6. `type="json"` on routes — use `type="jsonrpc"`
