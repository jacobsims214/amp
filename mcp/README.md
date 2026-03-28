# MCP Server for Odoo Project Management

Dockerized MCP server wrapping Odoo 19 API for agentic management platform.

## Configuration

Environment variables:
- `ODOO_URL`: Odoo server URL (default: http://host.docker.internal:8069)
- `ODOO_DB`: Database name
- `ODOO_USER`: Username
- `ODOO_PASSWORD`: Password
- `MCP_PORT`: Server port (default: 8000)

## Running Locally

```bash
# Build
docker build -t amp-mcp .

# Run
docker run -p 8000:8000 \
  -e ODOO_URL=http://host.docker.internal:8069 \
  -e ODOO_DB=odoo_db \
  -e ODOO_USER=admin \
  -e ODOO_PASSWORD=admin \
  amp-mcp
```

## MCP Tools

### Project Management
- `create_project`: Create new Odoo project
- `get_project`: Get project details
- `update_project`: Update project fields
- `list_projects`: List all projects
- `archive_project`: Archive a project

### Task/Epic/Story Management
- `create_epic`: Create epic (parent task)
- `create_story`: Create story under epic
- `create_task`: Create task under story
- `get_task`: Get task details
- `update_task`: Update task fields
- `assign_task`: Assign task to agent
- `update_task_status`: Update task state/stage
- `get_task_hierarchy`: Get full epic/story/task tree
- `add_task_comment`: Add status update/comment

### Knowledge Base
- `create_kb_entry`: Create knowledge base entry
- `search_kb`: Search knowledge base
- `link_kb_to_task`: Link KB entry to task
- `get_kb_context`: Get context for project/task

### Timesheet/Progress
- `log_time`: Log time on task
- `get_timesheet`: Get time entries for task/project

## API Format

JSON-RPC 2.0 over HTTP:
```json
{
  "jsonrpc": "2.0",
  "method": "call",
  "params": {
    "tool": "create_project",
    "arguments": {"name": "New Project"}
  },
  "id": 1
}
```

## Health Check

```
GET /health
```
