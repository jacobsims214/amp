# Odoo API Skill

This skill provides patterns for interacting with Odoo's XML-RPC and JSON-RPC APIs.

## Connection Patterns

### XML-RPC (External API)
```python
import xmlrpc.client

url = 'http://localhost:8069'
db = 'odoo_db'
username = 'admin'
password = 'admin'

# Authenticate
common = xmlrpc.client.ServerProxy(f'{url}/xmlrpc/2/common')
uid = common.authenticate(db, username, password, {})

# Execute methods
models = xmlrpc.client.ServerProxy(f'{url}/xmlrpc/2/object')
result = models.execute_kw(db, uid, password, 'res.partner', 'search_read', [[['is_company', '=', True]]], {'fields': ['name'], 'limit': 5})
```

### JSON-RPC (Internal/External)
```python
import requests

url = 'http://localhost:8069/jsonrpc'
headers = {'Content-Type': 'application/json'}

def call_odoo(method, params):
    payload = {
        'jsonrpc': '2.0',
        'method': 'call',
        'params': params,
        'id': 1
    }
    response = requests.post(url, json=payload, headers=headers)
    return response.json()

# Authentication
auth_params = {
    'service': 'common',
    'method': 'authenticate',
    'args': [db, username, password, {}]
}
uid = call_odoo('authenticate', auth_params)
```

## Standard Operations

### Search and Read
```python
# Search with domain
models.execute_kw(db, uid, password, 
    'project.project', 'search', 
    [[['name', 'ilike', 'My Project']]],
    {'limit': 10}
)

# Search and read
models.execute_kw(db, uid, password,
    'project.project', 'search_read',
    [[['state', '=', 'open']]],
    {'fields': ['name', 'user_id', 'date_start'], 'limit': 5}
)
```

### Create
```python
project_id = models.execute_kw(db, uid, password,
    'project.project', 'create',
    [{'name': 'New Project', 'description': 'Project description'}]
)
```

### Update
```python
models.execute_kw(db, uid, password,
    'project.project', 'write',
    [[project_id], {'name': 'Updated Project Name'}]
)
```

### Delete
```python
models.execute_kw(db, uid, password,
    'project.project', 'unlink',
    [[project_id]]
)
```

### Method Calls
```python
# Custom methods
models.execute_kw(db, uid, password,
    'project.project', 'message_post',
    [[project_id]],
    {'body': 'Status update', 'message_type': 'comment'}
)
```

## Common Odoo Fields

- `id`: Record ID
- `create_date`: Creation timestamp
- `write_date`: Last modification
- `create_uid`: Creator user ID
- `write_uid`: Last modifier user ID

## Error Handling

```python
try:
    result = models.execute_kw(...)
except xmlrpc.client.Fault as e:
    # Handle Odoo error
    print(f"Odoo error: {e.faultString}")
except Exception as e:
    # Handle connection/other errors
    print(f"Error: {e}")
```

## Best Practices

1. Always use `search_read` to reduce API calls
2. Batch operations with lists of IDs
3. Use context parameter for timezone/language
4. Handle authentication failures gracefully
5. Cache uid for session reuse
