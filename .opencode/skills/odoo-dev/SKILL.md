---
name: odoo-dev
description: How to install, update, and debug Odoo modules in this dev environment — CLI commands, container setup, and troubleshooting
---

# Odoo Dev Environment

## Container layout

| Container | Role | Port |
|-----------|------|------|
| `odoo-odoo-1` | Odoo 19 server | 8069 |
| `odoo-db-1` | PostgreSQL | 5432 (internal) |
| `amp-odoo-mcp` | Go MCP server | 8000 |

Module source is mounted into the Odoo container:
- Host: `/Users/jacobsims/development/simstech/amp/odoo-modules/`
- Container: `/mnt/extra-addons-2/`

DB credentials: host=`db`, user=`odoo`, password=`odoo`, db=`odoo19`

---

## Installing or updating a module

**Always use the Odoo CLI inside the container** — not the web UI for installs.
The web UI fails silently or shows stale errors from previous attempts.

```bash
# Install a module for the first time
docker exec odoo-odoo-1 odoo -d odoo19 \
  --db_host=db --db_user=odoo --db_password=odoo \
  --addons-path="/mnt/extra-addons-2,/mnt/extra-addons/,/usr/lib/python3/dist-packages/odoo/addons" \
  -i MODULE_NAME --stop-after-init

# Update an already-installed module
docker exec odoo-odoo-1 odoo -d odoo19 \
  --db_host=db --db_user=odoo --db_password=odoo \
  --addons-path="/mnt/extra-addons-2,/mnt/extra-addons/,/usr/lib/python3/dist-packages/odoo/addons" \
  -u MODULE_NAME --stop-after-init

# Update multiple modules at once
docker exec odoo-odoo-1 odoo -d odoo19 \
  --db_host=db --db_user=odoo --db_password=odoo \
  --addons-path="/mnt/extra-addons-2,/mnt/extra-addons/,/usr/lib/python3/dist-packages/odoo/addons" \
  -u amp_project,amp_knowledge --stop-after-init
```

After the CLI command completes, restart the server so the running instance picks up changes:
```bash
docker restart odoo-odoo-1 && sleep 12
```

---

## Validating a view before deploying

Always validate search views (and other views) against the RelaxNG schema before running install:

```bash
# Validate a search view
docker exec odoo-odoo-1 python3 -c "
from lxml import etree
rng = etree.RelaxNG(etree.parse('/usr/lib/python3/dist-packages/odoo/addons/base/rng/search_view.rng'))
doc = etree.parse('/mnt/extra-addons-2/MY_MODULE/views/MY_VIEWS.xml')
for record in doc.findall('.//record'):
    arch = record.find('.//field[@name=\"arch\"]')
    if arch is not None:
        for view in arch:
            if view.tag == 'search':
                valid = rng.validate(view)
                print('search:', 'VALID' if valid else [str(e) for e in rng.error_log])
"

# Available RNG schemas:
# /usr/lib/python3/dist-packages/odoo/addons/base/rng/search_view.rng
# /usr/lib/python3/dist-packages/odoo/addons/base/rng/list_view.rng
# /usr/lib/python3/dist-packages/odoo/addons/base/rng/common.rng
```

---

## Getting the real error from a failed install

The Odoo web UI error says "Invalid view ... line 71" — that line number is the `<record>` tag, not the actual problem.
Get the real error with `--log-handler`:

```bash
docker exec odoo-odoo-1 odoo -d odoo19 \
  --db_host=db --db_user=odoo --db_password=odoo \
  --addons-path="/mnt/extra-addons-2,/mnt/extra-addons/,/usr/lib/python3/dist-packages/odoo/addons" \
  --log-handler="odoo.tools.view_validation:DEBUG" \
  -i MODULE_NAME --stop-after-init 2>&1 | grep -E "ERROR|RELAXNG|Invalid|ParseError|WARNING.*valid"
```

The `odoo.tools.view_validation` logger emits the actual RelaxNG errors like:
```
RELAXNG_ERR_INVALIDATTR: Invalid attribute expand for element group
RELAXNG_ERR_EXTRACONTENT: Element search has extra content: field
```

---

## Checking module state

```python
import xmlrpc.client
url = 'http://localhost:8069'
db = 'odoo19'
common = xmlrpc.client.ServerProxy(f'{url}/xmlrpc/2/common')
uid = common.authenticate(db, 'admin', 'admin', {})
models = xmlrpc.client.ServerProxy(f'{url}/xmlrpc/2/object')

result = models.execute_kw(db, uid, 'admin', 'ir.module.module', 'search_read',
    [[['name', 'in', ['amp_project', 'amp_knowledge']]]], 
    {'fields': ['name', 'state', 'installed_version']})
for r in result:
    print(r)
```

---

## MCP server (Go)

```bash
# Rebuild and restart MCP after Go code changes
docker compose up -d --build   # from /Users/jacobsims/development/simstech/amp/

# Check MCP logs
docker logs amp-odoo-mcp --tail=20

# Test an MCP tool
SSE=$(mktemp)
curl -s -N http://localhost:8000/sse > $SSE &
sleep 1
SID=$(grep -o 'sessionId=[^"]*' $SSE | head -1 | cut -d= -f2)
curl -s -X POST "http://localhost:8000/message?sessionId=$SID" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"amp_health_check","arguments":{}}}' \
  | python3 -m json.tool
kill %1; rm $SSE
```

---

## AMP module addons path

The running Odoo server uses this addons path (from `/etc/odoo/odoo.conf`):
```
/mnt/extra-addons/
/mnt/extra-addons/AvWare/
/mnt/extra-addons/enterprise/odoo/addons
/usr/lib/python3/dist-packages/odoo/addons
```

**`/mnt/extra-addons-2` is NOT in the running server's config** — that's why `-i`/`-u` CLI commands need `--addons-path` explicitly. After a CLI update, the running server picks up Python model changes on restart but the DB changes are persisted immediately.
