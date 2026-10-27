package mcp

// Instructions are sent to MCP clients when they connect (the initialize
// response); clients typically put them in the model's context. They carry
// the few rules an agent must know before planning in AMP — above all, that
// the dependency graph drives everything.
const Instructions = `AMP tracks agentic work as projects → epics → stories → tickets (tasks), and runs it as a dependency graph (DAG).

AMP's tools (amp_*) are in your tool list: call them by name, no need to search for them. Some harnesses prefix them with the server name (in opencode: amp_amp_list_tasks). Orient with amp_list_tasks (a compact board) and amp_get_plan (epics and stories); read a ticket with amp_get_task, adding include=["comments","history","runs"] for its story.

DEPENDENCIES ARE CRITICAL. AMP dispatches every ticket whose dependencies are complete, in parallel. A ticket with no dependencies is ready immediately. If a ticket needs another ticket's output (a schema, an API, a design, a fix, a decision) and that dependency is missing, it will be started too early and fail.

When planning:
1. Create the epics and stories, then decide the order of the work: for every ticket, which tickets must finish first? Dependencies often cross stories and epics (the API before the UI that calls it).
2. Create related tickets with amp_create_tasks: give each a ref and list what it depends_on, so the whole graph is wired in one call. Leave depends_on empty only when a ticket truly needs nothing.
3. Run amp_check_plan before dispatching anything. It flags tickets connected to nothing and too much work that can start at once.

Fixing a plan — never delete and re-create tickets to fix them:
- wrong or missing dependencies: amp_set_task_dependencies (full list, or add/remove)
- wrong text, acceptance criteria, priority, assignee: amp_update_task; epics and stories: amp_update_epic / amp_update_story
- ticket in the wrong story: amp_move_task; story in the wrong epic: amp_move_story
- unwanted work: amp_delete_task / amp_delete_story / amp_delete_epic (tickets that depended on it are updated)

QUESTIONS. A subagent can't wait for an answer. If you can't go on without a person (a decision, missing information, access), comment what you found, call amp_block_task with the question as the reason, and stop. The ticket shows as a question in AMP and in amp_list_tasks' questions list. Whoever answers (a person in the AMP UI, or the orchestrator with amp_answer_question) puts the answer on the ticket as a comment and the ticket goes back to ready; the next worker dispatched to it reads the comments first.

DESIGN. Before any UI, UX or visual work (mockups, screens, styling, diagrams, images), call amp_design_context(project_id): it lists the three layers to follow, most general first: AMP's design guide (the process and quality bar), the org's design system (org KB, design-system/*) and the project's additions (project KB, design-system/*). The more specific layer wins. Design systems are built with AMP's design system process, never by hand: amp_design_system_start (scope "org" for the org's, in its Design System project; scope "project" for a product's layer once the org has published) plans the version from a fixed template of phases, each ending in an artifact the user approves, and amp_design_system_publish (the only way design-system/ docs are written) publishes it once every phase is approved. See amp_guide topic "design/building-a-design-system.md".

PEOPLE IN THE LOOP. A step only a person can do (review and merge a PR, a sign-off, access agents don't have, a decision, someone outside the team, offline work) is a ticket too: performer "human" with its owner as the person, wired into depends_on so the agent work after it waits for them. Agents never dispatch it. Its owner starts and finishes it in AMP. amp_list_tasks' waiting_on_people (Waiting on you) says who the plan is waiting on and for what: approve, answer, merge (Merge needed) or do.

MERGES. A ticket completed with a PR or a pushed branch is Merge needed, and the tickets after it wait until the merge is recorded (amp_report_merged, the GitHub webhook, or Mark merged in AMP). For an epic or story whose tickets merge into an integration branch before one PR to main, set metadata integration_branch: "<branch>" on it; agents report that branch (merged into it) or base_branch=<it> (a PR into it), and the next ticket starts on completion. The ticket that opens the integration PR to main still holds its dependents. On a ticket that needs a PR open, not merged (a review), set waits_for_merge: false. Neither key needs re-approval.

SHOW BEFORE YOU BUILD. When the user should see something before it's built (a new screen or flow, a landing page, a diagram, a visual design), plan a design ticket first and make the build tickets depend on it. The designer publishes the mockup with amp_publish_artifact (one self-contained HTML file, an SVG, an image, or a document), then blocks its ticket asking for review at the artifact's url, and stops. People review in AMP (Approve, or Request changes); when the user gives a verdict in your chat, record it with amp_review_artifact. Either way the verdict lands on the ticket and it goes back to ready: dispatch it again and the designer revises (a new version of the same artifact) or completes it. Builders read the approved version with amp_get_artifact.

LEARNING AMP. amp_guide is AMP's own guide, shipped with AMP. When your user is new to AMP, asks how something works, or asks what to do next, call amp_guide (no topic for the onboarding script) and teach them by doing it with them in their real projects. Look a feature up with amp_guide before saying AMP can't do it.

Other rules: a ticket's state (backlog/blocked) is derived from its dependencies — don't set it to work around a missing dependency. amp_list_tasks shows your own work unless you pass owner, compactly unless you pass format="full". The project knowledge base (amp_kb_*) holds what agents learned; search it before starting.`
