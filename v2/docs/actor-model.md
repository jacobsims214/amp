# The Actor Model — What It Is and How We Use It

## What Is Erlang?

Erlang is a programming language created at Ericsson in the 1980s to build telephone switching systems. Those systems had hard requirements that most software never faces:

- **Never go down** — a phone switch must stay up 24/7/365
- **Handle millions of concurrent connections** — thousands of calls at once
- **Recover from failures automatically** — a crashed subsystem must restart without taking the whole switch down
- **Update live without restarting** — ship fixes while calls are in progress

To solve these problems, Ericsson built Erlang around the **Actor Model** as its core concurrency primitive. The result was a runtime capable of running millions of lightweight processes on a single machine, each fully isolated from the others, communicating only by message passing.

The BEAM VM (Erlang's runtime) has been proven to hit **99.9999999% uptime** (nine nines) in production telecom systems.

## What Is the Actor Model?

The Actor Model is a mathematical model of concurrent computation invented by Carl Hewitt at MIT in 1973. The core idea:

> **Everything is an actor.** An actor is the fundamental unit of computation.

### The Three Rules of an Actor

In response to a message, an actor may:

1. **Send** a finite number of messages to other actors it knows about
2. **Create** a finite number of new child actors
3. **Change its behavior** — decide how to handle the *next* message it receives

That's it. No shared memory. No locks. No mutexes. All state is private to each actor.

### Why This Matters

Traditional concurrent programming uses shared memory protected by locks:

```
Thread A: lock(mutex) → read state → modify → unlock(mutex)
Thread B: lock(mutex) → read state → modify → unlock(mutex)  // waits for A
```

Problems:
- Deadlock: A waits for B, B waits for A
- Race conditions: forget a lock, corrupt data
- Lock contention kills performance under load

The Actor Model eliminates shared state entirely:

```
Actor A owns its state exclusively.
Anything outside must send a message to change it.
Actor A processes one message at a time — no concurrent modification.
```

No locks needed because no sharing ever happens.

### Actors in Go

Go goroutines + channels are a natural fit for the Actor Model:

```go
// An actor is a goroutine with a private mailbox (channel)
type Actor struct {
    mailbox chan Message
    state   privateState   // nobody else touches this
}

// The actor loop — processes one message at a time
func (a *Actor) run() {
    for msg := range a.mailbox {
        a.handleMessage(msg)  // state changes happen here, safely
    }
}

// Sending a message is non-blocking (fire and forget)
actor.mailbox <- Message{Type: "DoThing", Payload: data}
```

This is exactly what Erlang processes do, but expressed in Go idioms.

## The Erlang Process Model vs. Go Actors

| Erlang | Our Go Implementation |
|--------|----------------------|
| Spawning a process (`spawn/1`) | Starting a goroutine running an actor loop |
| Process mailbox | Buffered channel (`chan Message`) |
| Pattern matching on messages | Switch on `msg.Type` |
| Process ID (PID) | Actor reference struct with send method |
| `link` / `monitor` | Supervisor watching child actors |
| Supervisor tree | `Supervisor` struct managing a registry of actors |
| `let it crash` philosophy | Defer/recover in actor loop, restart via supervisor |
| OTP GenServer | Our `Actor` interface with `Receive(msg)` |

## Supervision Trees

Erlang's killer feature is **supervision trees**. If a process crashes, its supervisor restarts it. Supervisors are themselves supervised. The tree ensures that:

1. A crash is **contained** — it doesn't propagate upward unless the supervisor also fails
2. The system **self-heals** — the supervisor restarts the failed child with fresh state
3. **Cascading failures** are impossible by design

```
                    RootSupervisor
                   /              \
          ProjectSupervisor    SystemSupervisor
          /     |     \
   Proj1Actor Proj2Actor Proj3Actor
   /    \
Task1  Task2
```

If Task1 panics:
- Task1's goroutine terminates
- ProjectSupervisor detects it (via done channel)
- Supervisor restarts Task1 with persisted state loaded from PSQL
- Everything else keeps running

## How AMP v2 Uses Actors

### The Actor Hierarchy

```
ActorSystem (root)
├── Supervisor
│   ├── ProjectActor {id: "proj-1"}      ← one per project
│   │   ├── EpicActor {id: "epic-1"}     ← one per epic
│   │   │   ├── StoryActor {id: "st-1"}  ← one per story
│   │   │   │   ├── TaskActor {id: "t-1"} ← one per task
│   │   │   │   └── TaskActor {id: "t-2"}
│   │   │   └── StoryActor {id: "st-2"}
│   │   └── EpicActor {id: "epic-2"}
│   └── ProjectActor {id: "proj-2"}
```

### Message Flow: MCP Tool Call → Actor → DB → Real-time Push

```
Agent calls amp_create_task(...)
        ↓
MCP Server receives tool call
        ↓
Looks up ProjectActor for project_id
        ↓
Sends CreateTask{...} message to ProjectActor mailbox
        ↓
ProjectActor processes message:
  1. Validates request
  2. Creates TaskActor child
  3. Persists to PostgreSQL
  4. Broadcasts TaskCreated event to SSE hub
  5. Replies to caller with task data
        ↓
SSE Hub pushes event to any connected Next.js frontends
        ↓
MCP returns result to agent
```

### Actor State Machines

Each TaskActor maintains a state machine:

```
backlog → in_progress → completed
   ↓            ↓
blocked      blocked
   ↓            ↓
backlog      in_progress
```

State transitions are messages:
- `DispatchTask{AgentID}` → `backlog → in_progress`
- `CompleteTask{}` → `in_progress → completed`
- `BlockTask{Reason}` → `* → blocked`

The actor enforces valid transitions and rejects invalid ones.

### Why Actors Power the Real-time Frontend

In v1, the frontend had to poll Odoo for updates. With actors:

1. Every state change happens **inside** an actor
2. The actor publishes an event to the SSE hub **as part of processing the message**
3. The Next.js frontend has a persistent SSE connection
4. Changes appear **instantly**, no polling needed

The actor is the single source of truth for real-time state. PostgreSQL is its durable backing store for restart/recovery.

## The "Let It Crash" Philosophy

Erlang's most counterintuitive principle: **don't defensively program around every possible error**. Instead:

- Write actor logic for the **happy path**
- If something goes wrong, the actor **panics/crashes**
- The **supervisor** catches it and **restarts with clean state**

In practice this means:
- Actor code is simpler (no defensive nil-checks everywhere)
- Errors are surfaced immediately, not silently swallowed
- The system recovers automatically
- Logs capture the crash with full context

Our Go implementation:
```go
func (a *TaskActor) run() {
    defer func() {
        if r := recover(); r != nil {
            // log the crash
            a.supervisor.actorCrashed(a.id, fmt.Errorf("%v", r))
        }
    }()
    for msg := range a.mailbox {
        a.receive(msg)
    }
}
```

## Key Properties We Get From This Architecture

| Property | How We Get It |
|----------|--------------|
| Real-time UI updates | Actors broadcast SSE events on every state change |
| No race conditions | Each actor owns its state exclusively |
| Crash isolation | Supervisors contain and restart failed actors |
| Horizontal scalability | Actor registry can shard across nodes |
| Consistent state | Actors serialize all access; DB is write-behind cache |
| DAG dependencies | TaskActor checks dependency set before allowing dispatch |
| Audit trail | Every message processed is logged with timestamp |
