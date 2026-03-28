# Odoo Project Module Skill

Understanding Odoo 19's project module structure and workflows for AMP integration.

## Core Models

### project.project (Projects)
```python
# Key fields
- name: Project name (required)
- description: Project description
- user_id: Project manager (res.users)
- partner_id: Customer (res.partner)
- date_start: Start date
- date: End date
- state: 'open', 'close', 'cancel'
- task_count: Computed task count
- task_ids: One2many to project.task
- label_tasks: Custom task label
```

### project.task (Tasks)
```python
# Key fields
- name: Task name (required)
- project_id: Parent project (project.project)
- user_ids: Assigned users (Many2many res.users)
- stage_id: Current stage (project.task.type)
- description: Task description
- priority: '0'=Low, '1'=High
- state: '01_in_progress', '02_changes_requested', etc.
- date_deadline: Due date
- planned_hours: Estimated hours
- effective_hours: Logged hours (via timesheets)
- parent_id: Parent task (for subtasks/epics)
- child_ids: Subtasks
- tag_ids: Tags (project.tags)
- kanban_state: 'normal', 'done', 'blocked'
```

### project.task.type (Stages)
```python
# Kanban stages
- name: Stage name
- project_ids: Projects using this stage
- sequence: Stage order
- fold: Folded in kanban
- rating_template_id: Rating email template
```

### project.tags
```python
# Task categorization
- name: Tag name
- color: Color index (0-11)
```

## Standard Workflows

### Project Lifecycle
```
draft → open → close/cancel
```

### Task States (Odoo 19)
```
01_in_progress → 02_changes_requested → 03_approved → 1_done
                ↓
                04_waiting_normal → 1_canceled
```

### Kanban States
```
normal → done (green)
       → blocked (red)
```

## Epic/Story Hierarchy Pattern

Odoo 19 supports parent-child relationships for epics and stories:

```python
# Create Epic (parent task)
epic_id = models.execute_kw(db, uid, password,
    'project.task', 'create',
    [{
        'name': 'Epic: User Authentication',
        'project_id': project_id,
        'description': 'Implement authentication system'
    }]
)

# Create Story (child task)
story_id = models.execute_kw(db, uid, password,
    'project.task', 'create',
    [{
        'name': 'Story: Login Page',
        'project_id': project_id,
        'parent_id': epic_id,  # Links to epic
        'description': 'Create login interface'
    }]
)

# Create Task (child of story)
task_id = models.execute_kw(db, uid, password,
    'project.task', 'create',
    [{
        'name': 'Task: Form Validation',
        'project_id': project_id,
        'parent_id': story_id,
        'description': 'Implement form validation'
    }]
)
```

## Task Assignment Patterns

### Assign to Agent
```python
# Find or create agent user
agent_user_id = models.execute_kw(db, uid, password,
    'res.users', 'search',
    [[['login', '=', 'agent.subagent1']]],
    {'limit': 1}
)[0]

# Assign task
models.execute_kw(db, uid, password,
    'project.task', 'write',
    [[task_id], {'user_ids': [[6, 0, [agent_user_id]]]}]
)
```

### Update Progress
```python
# Update kanban state
models.execute_kw(db, uid, password,
    'project.task', 'write',
    [[task_id], {'kanban_state': 'done'}]
)

# Move to next stage
next_stage_id = 5  # Get from project.task.type search
models.execute_kw(db, uid, password,
    'project.task', 'write',
    [[task_id], {'stage_id': next_stage_id}]
)
```

## Common Operations

### Get Project Tasks
```python
# Get all tasks in project
tasks = models.execute_kw(db, uid, password,
    'project.task', 'search_read',
    [[['project_id', '=', project_id]]],
    {'fields': ['name', 'user_ids', 'stage_id', 'state', 'parent_id']}
)
```

### Get Task Hierarchy
```python
# Get epic with all children
def get_task_hierarchy(task_id):
    task = models.execute_kw(db, uid, password,
        'project.task', 'read',
        [[task_id]],
        {'fields': ['name', 'child_ids', 'parent_id']}
    )[0]
    
    if task['child_ids']:
        children = []
        for child_id in task['child_ids']:
            children.append(get_task_hierarchy(child_id))
        task['children'] = children
    
    return task
```

### Add Timesheet Entry
```python
models.execute_kw(db, uid, password,
    'account.analytic.line', 'create',
    [{
        'task_id': task_id,
        'project_id': project_id,
        'name': 'Worked on feature',
        'unit_amount': 2.5,  # Hours
        'user_id': agent_user_id
    }]
)
```

## Custom Fields for AMP

When extending project module:

```python
# Add to project.task
task_type = fields.Selection([
    ('epic', 'Epic'),
    ('story', 'Story'),
    ('task', 'Task'),
    ('bug', 'Bug')
], string='Task Type')

agent_id = fields.Char(string='Agent ID', help='OpenCode agent identifier')
context_data = fields.Text(string='Context Data', help='JSON context from agent')
```

## Best Practices

1. Use parent_id for epic/story hierarchy
2. Set kanban_state to signal blockers
3. Use tags for categorization
4. Log time via timesheets for tracking
5. Post messages for status updates
6. Archive (active=False) instead of delete
