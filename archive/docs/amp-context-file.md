# AMP Context File (.amp.json)

The `.amp.json` file lives in each project directory and stores local context for agents.

## Purpose

- **Project ID**: Links directory to Odoo AMP project
- **Session Tracking**: Last interaction timestamp
- **Active Work**: Current epic/story/task IDs
- **KB Tracking**: Related knowledge entries
- **Custom Context**: Agent-specific data

## Schema

```json
{
  "version": "1.0.0",
  "project_id": 123,              // Odoo amp.project ID (required)
  "project_code": "my-project",    // Project code/directory name
  "project_name": "My Project",    // Human-readable name
  "created_at": "2026-03-28T10:00:00Z",
  "last_session": "2026-03-28T15:30:00Z",
  "current_epic_id": 456,          // Currently active epic
  "current_story_id": 789,         // Currently active story
  "active_task_ids": [101, 102],   // Tasks being worked
  "kb_entry_ids": [201, 202],      // Related KB entries
  "context": {                     // Custom agent context
    "key_decisions": [],
    "active_agents": [],
    "notes": ""
  }
}
```

## Usage

**Manager on Startup:**
1. Check for `.amp.json`
2. If exists: Load project_id, verify with MCP
3. If not: Create project in Odoo, write `.amp.json`

**Workers:**
1. Read `.amp.json` to get project_id
2. Get assigned task_id from manager context
3. Use both to interact with MCP

**Updates:**
- Manager updates when creating new epics/stories
- Workers update when starting/completing tasks
- All agents update `last_session` timestamp

## Example Flow

```python
# Manager startup
def initialize_project():
    import json
    import os
    
    if os.path.exists('.amp.json'):
        with open('.amp.json', 'r') as f:
            ctx = json.load(f)
        
        # Verify with Odoo
        project = mcp_call("get_project", {"project_id": ctx["project_id"]})
        if project:
            print(f"Loaded project: {project['name']}")
            return ctx["project_id"]
    
    # Create new project
    project = mcp_call("create_project", {
        "name": os.path.basename(os.getcwd()),
        "code": os.path.basename(os.getcwd())
    })
    
    # Write context file
    ctx = {
        "version": "1.0.0",
        "project_id": project["project_id"],
        "project_code": project["code"],
        "project_name": project["name"],
        "created_at": datetime.now().isoformat(),
        "last_session": datetime.now().isoformat(),
        "context": {}
    }
    
    with open('.amp.json', 'w') as f:
        json.dump(ctx, f, indent=2)
    
    return project["project_id"]
```
