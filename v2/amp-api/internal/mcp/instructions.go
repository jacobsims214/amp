package mcp

// Instructions are sent to MCP clients when they connect (the initialize
// response); clients typically put them in the model's context. They carry
// the few rules an agent must know before planning in AMP — above all, that
// the dependency graph drives everything.
const Instructions = `AMP tracks agentic work as projects → epics → stories → tickets (tasks), and runs it as a dependency graph (DAG).

DEPENDENCIES ARE CRITICAL. AMP dispatches every ticket whose dependencies are complete, in parallel. A ticket with no dependencies is ready immediately. If a ticket needs another ticket's output (a schema, an API, a design, a fix, a decision) and that dependency is missing, it will be started too early and fail.

When planning:
1. Create the epics and stories, then decide the order of the work: for every ticket, which tickets must finish first? Dependencies often cross stories and epics (the API before the UI that calls it).
2. Create related tickets with amp_create_tasks: give each a ref and list what it depends_on, so the whole graph is wired in one call. With amp_create_task, always pass dependency_ids unless the ticket truly needs nothing.
3. Run amp_check_plan before dispatching anything. It flags tickets connected to nothing and too much work that can start at once.

Fixing a plan — never delete and re-create tickets to fix them:
- wrong or missing dependencies: amp_set_task_dependencies (full list, or add/remove)
- wrong text, acceptance criteria, priority, assignee: amp_update_task; epics and stories: amp_update_epic / amp_update_story
- ticket in the wrong story: amp_move_task; story in the wrong epic: amp_move_story
- unwanted work: amp_delete_task / amp_delete_story / amp_delete_epic (tickets that depended on it are updated)

QUESTIONS. A subagent can't wait for an answer. If you can't go on without a person (a decision, missing information, access), comment what you found, call amp_block_task with the question as the reason, and stop. The ticket shows as a question in AMP and in amp_list_tasks' questions list. Whoever answers (a person in the AMP UI, or the orchestrator with amp_answer_question) puts the answer on the ticket as a comment and the ticket goes back to ready; the next worker dispatched to it reads the comments first.

Other rules: a ticket's state (backlog/blocked) is derived from its dependencies — don't set it to work around a missing dependency. amp_list_tasks shows your own work unless you pass owner. The project knowledge base (amp_kb_*) holds what agents learned; search it before starting.`
