---
description: Platform skills for all AMP agents - project context, KB usage, status updates
mode: skill
---

# AMP Platform Skills

Skills that ALL AMP agents (manager and workers) must use consistently.

## 1. Project Context File (.amp.json)

Each project directory contains `.amp.json` for local context:

```json
{
  "version": "1.0.0",
  "project_id": 123,
  "project_code": "my-project",
  "project_name": "My Project",
  "created_at": "2026-03-28T10:00:00Z",
  "last_session": "2026-03-28T15:30:00Z",
  "current_epic_id": 456,
  "current_story_id": 789,
  "active_task_ids": [101, 102, 103],
  "kb_entry_ids": [201, 202],
  "context": {
    "key_decisions": ["Use PostgreSQL", "JWT auth"],
    "active_agents": ["amp-worker-1", "amp-worker-2"]
  }
}
```

### Reading Project Context

```python
def get_project_context():
    """Get project context from .amp.json"""
    import json
    try:
        with open('.amp.json', 'r') as f:
            return json.load(f)
    except FileNotFoundError:
        return None

# Usage
ctx = get_project_context()
if ctx:
    project_id = ctx['project_id']
    current_epic = ctx.get('current_epic_id')
```

### Writing Project Context

```python
def update_project_context(updates):
    """Update .amp.json with new context"""
    import json
    from datetime import datetime
    
    ctx = get_project_context() or {}
    ctx.update(updates)
    ctx['last_session'] = datetime.now().isoformat()
    
    with open('.amp.json', 'w') as f:
        json.dump(ctx, f, indent=2)

# Usage
update_project_context({
    'current_epic_id': epic_id,
    'active_task_ids': [task1_id, task2_id]
})
```

## 2. Knowledge Base Usage

### Search KB Before Work

```python
def get_relevant_context(project_id, query, task_id=None):
    """Search KB for relevant context"""
    results = mcp_call("search_kb", {
        "query": query,
        "project_id": project_id,
        "limit": 5
    })
    
    # Also get context linked to parent story/epic
    if task_id:
        task = mcp_call("get_task", {"task_id": task_id})
        story_id = task.get('story_id')
        if story_id:
            story = mcp_call("get_story", {"story_id": story_id})
            # Search KB for story-related entries
            story_results = mcp_call("search_kb", {
                "query": story['name'],
                "project_id": project_id,
                "limit": 3
            })
            results.extend(story_results)
    
    return results

# Usage before starting task
context = get_relevant_context(project_id, "authentication JWT", task_id)
```

### Create KB Entry

```python
def create_knowledge_entry(title, content, project_id, epic_id=None, 
                           story_id=None, task_id=None, tags=None):
    """Create KB entry and link to work items"""
    kb = mcp_call("create_kb_entry", {
        "title": title,
        "content": content,
        "project_id": project_id,
        "epic_id": epic_id,
        "story_id": story_id,
        "task_id": task_id,
        "tags": tags or []
    })
    
    # Update .amp.json to track this KB entry
    ctx = get_project_context()
    if ctx:
        kb_ids = ctx.get('kb_entry_ids', [])
        kb_ids.append(kb['entry_id'])
        update_project_context({'kb_entry_ids': kb_ids})
    
    return kb

# Usage
kb = create_knowledge_entry(
    title="Decision: Use Redis for Sessions",
    content="After evaluating options, decided to use Redis...",
    project_id=project_id,
    task_id=current_task_id,
    tags=["decision", "architecture", "redis"]
)
```

### Link KB to Task

```python
def link_kb_to_task(task_id, kb_entry_ids):
    """Link KB entries to task context"""
    import json
    
    task = mcp_call("get_task", {"task_id": task_id})
    context = json.loads(task.get('context_data') or '{}')
    
    existing = context.get('kb_entries', [])
    existing.extend(kb_entry_ids)
    context['kb_entries'] = list(set(existing))  # Deduplicate
    
    mcp_call("update_task", {
        "task_id": task_id,
        "context_data": json.dumps(context)
    })
```

## 3. Status Update Patterns

### Worker Status Updates

```python
def report_progress(task_id, message, include_context=False):
    """Report progress on task"""
    body = message
    
    if include_context:
        task = mcp_call("get_task", {"task_id": task_id})
        body += f"\n\nCurrent state: {task['state']}"
        body += f"\nDAG Level: {task.get('dag_level', 0)}"
        if task.get('dependency_count'):
            body += f"\nDependencies: {task['dependency_count']}"
    
    mcp_call("add_task_comment", {
        "task_id": task_id,
        "body": body
    })

# Usage every 30 mins or on state change
report_progress(task_id, "✅ Completed database schema design", include_context=True)
```

### Manager Monitoring

```python
def monitor_and_dispatch(project_id):
    """Manager polls for ready tasks and dispatches"""
    import time
    
    while True:
        # Get dashboard
        dashboard = mcp_call("get_project_dashboard", {"project_id": project_id})
        
        # Check for blocked tasks
        blocked = dashboard.get('blocked_tasks', [])
        if blocked:
            # Alert human
            print(f"⚠️ {len(blocked)} blocked tasks need attention!")
            for task in blocked[:3]:
                print(f"  - {task['name']} (Agent: {task['agent']})")
        
        # Get ready tasks
        ready = mcp_call("get_ready_tasks", {"project_id": project_id})
        ready_tasks = ready.get('ready_tasks', [])
        
        if ready_tasks:
            print(f"🚀 Dispatching {len(ready_tasks)} ready tasks...")
            for task in ready_tasks:
                mcp_call("dispatch_task", {
                    "task_id": task['id'],
                    "agent_id": "amp-worker"
                })
        
        # Report status
        total = dashboard['counts']['tasks']
        completed = dashboard['by_state']['completed']
        print(f"📊 Progress: {completed}/{total} tasks ({dashboard['progress']:.1f}%)")
        
        # Check if done
        in_progress = dashboard['by_state']['in_progress']
        ready_count = len(ready_tasks)
        if completed == total and in_progress == 0 and ready_count == 0:
            print("✅ All work complete!")
            break
        
        time.sleep(30)  # Poll every 30 seconds
```

## 4. Error Handling

```python
def safe_mcp_call(tool_name, params, retries=3):
    """Call MCP with retry logic"""
    import time
    
    for i in range(retries):
        try:
            return mcp_call(tool_name, params)
        except Exception as e:
            if i < retries - 1:
                time.sleep(2 ** i)  # Exponential backoff
                continue
            
            # Log error to KB
            project_ctx = get_project_context()
            if project_ctx:
                create_knowledge_entry(
                    title=f"Error: {tool_name} failed",
                    content=f"Error: {str(e)}\n\nParams: {params}",
                    project_id=project_ctx['project_id'],
                    tags=["error", tool_name]
                )
            raise
```

## 5. Human Communication Patterns

### Manager to Human

```python
def format_status_for_human(dashboard):
    """Format dashboard data for human consumption"""
    return f"""
📊 Project: {dashboard['project']['name']}

Progress: {dashboard['progress']:.0f}% Complete
{dashboard['by_state']['completed']}/{dashboard['counts']['tasks']} tasks done

Active Work:
• In Progress: {dashboard['by_state']['in_progress']}
• Ready to Start: {dashboard['by_state']['ready']}
• Blocked: {dashboard['by_state']['blocked']}

Active Agents: {dashboard['agents']['active']}
{', '.join(dashboard['agents']['list']) if dashboard['agents']['list'] else 'None'}

🚫 Blocked Tasks:
{chr(10).join([f"  • {t['name']} - {t.get('block_reason', 'No reason')}" 
               for t in dashboard.get('blocked_tasks', [])[:5]])}

Next Actions:
{chr(10).join([f"  • Dispatch: {t['name']}" 
               for t in dashboard.get('ready_tasks', [])[:3]])}
"""
```

## Summary Checklist

**On Every Task**:
- [ ] Read `.amp.json` for project context
- [ ] Search KB for relevant info
- [ ] Read task description fully
- [ ] Update status appropriately
- [ ] Comment progress regularly
- [ ] Create KB entry for findings
- [ ] Update `.amp.json` context

**Manager Specific**:
- [ ] Poll for ready tasks every 30s
- [ ] Dispatch in parallel
- [ ] Monitor blocked tasks
- [ ] Report status to human
- [ ] Update `.amp.json` active_task_ids

**Worker Specific**:
- [ ] Report start → in_progress
- [ ] Comment every 30 mins
- [ ] Block with clear reason if stuck
- [ ] Call complete endpoint when done
