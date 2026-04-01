# AMP MCP Server - Go Rewrite Plan

## Overview
Rewrite the Python FastAPI MCP server to Go using Hexagonal Architecture (Ports & Adapters pattern) with proper MCP protocol support via SSE.

## Architecture: Hexagonal (Clean Architecture)

```
┌─────────────────────────────────────────────────────────────┐
│                    Infrastructure Layer                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   MCP HTTP   │  │ Odoo XML-RPC │  │   Config/Env     │  │
│  │   Adapter    │  │   Adapter    │  │                  │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────────────┘  │
│         │                 │                                 │
│         └─────────────────┘                                 │
│                   │                                         │
└───────────────────┼─────────────────────────────────────────┘
                    │ (driven by interfaces)
┌───────────────────┼─────────────────────────────────────────┐
│                   │            Application Layer             │
│                   │                                          │
│         ┌─────────▼──────────┐                              │
│         │   MCP Protocol     │                              │
│         │     Handlers       │                              │
│         └─────────┬──────────┘                              │
│                   │                                         │
│         ┌─────────▼──────────┐                              │
│         │   Use Cases        │                              │
│         │ (Project, Epic,    │                              │
│         │  Story, Task, KB)  │                              │
│         └─────────┬──────────┘                              │
│                   │                                         │
└───────────────────┼─────────────────────────────────────────┘
                    │ (driven by interfaces)
┌───────────────────┼─────────────────────────────────────────┐
│                   │            Domain Layer                  │
│                   │                                          │
│         ┌─────────▼──────────┐                              │
│         │   Domain Models    │                              │
│         │ (Project, Epic,    │                              │
│         │  Story, Task, KB)  │                              │
│         └────────────────────┘                              │
│                   │                                         │
│         ┌─────────▼──────────┐                              │
│         │   Repository       │                              │
│         │   Interfaces       │                              │
│         └────────────────────┘                              │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Layer Responsibilities

### Domain Layer
- **Models**: Pure business entities (Project, Epic, Story, Task, KBEntry)
- **Repository Interfaces**: Define what operations are possible (ports)
- **Value Objects**: Immutable data structures

### Application Layer
- **Use Cases**: Business logic orchestration
- **MCP Protocol Handlers**: Implement MCP protocol via mark3labs/mcp-go
- **DTOs**: Data transfer objects for boundaries

### Infrastructure Layer
- **Odoo Adapter**: XML-RPC client implementation
- **MCP HTTP Adapter**: SSE transport for MCP protocol
- **Config**: Environment variable handling

## Project Structure

```
mcp-go/
├── cmd/
│   └── server/
│       └── main.go              # Application entry point
├── internal/
│   ├── domain/
│   │   ├── models/              # Business entities
│   │   │   ├── project.go
│   │   │   ├── epic.go
│   │   │   ├── story.go
│   │   │   ├── task.go
│   │   │   └── kb.go
│   │   └── repository/          # Repository interfaces (ports)
│   │       └── interfaces.go
│   ├── application/
│   │   ├── usecases/            # Business logic
│   │   │   ├── project.go
│   │   │   ├── epic.go
│   │   │   ├── story.go
│   │   │   ├── task.go
│   │   │   ├── kb.go
│   │   │   └── dashboard.go
│   │   └── mcp/                 # MCP protocol implementation
│   │       ├── handlers.go
│   │       └── tools.go
│   └── infrastructure/
│       ├── odoo/                # Odoo XML-RPC adapter
│       │   ├── client.go
│       │   ├── auth.go
│       │   └── models.go
│       ├── config/
│       │   └── config.go
│       └── http/
│           └── server.go
├── pkg/
│   └── utils/
│       └── helpers.go
├── go.mod
├── go.sum
├── Dockerfile
└── README.md
```

## MCP Protocol Tools (mapping from REST endpoints)

### Projects
- `amp_create_project` - POST /projects
- `amp_list_projects` - GET /projects
- `amp_get_project` - GET /projects/{id}
- `amp_update_project` - PUT /projects
- `amp_get_project_by_code` - GET /projects/by-code/{code}

### Epics
- `amp_create_epic` - POST /epics
- `amp_get_epic` - GET /epics/{id}
- `amp_list_epics` - GET /projects/{id}/epics
- `amp_update_epic_dag` - PUT /epics/{id}/dag

### Stories
- `amp_create_story` - POST /stories
- `amp_get_story` - GET /stories/{id}
- `amp_list_stories` - GET /epics/{id}/stories

### Tasks
- `amp_create_task` - POST /tasks
- `amp_get_task` - GET /tasks/{id}
- `amp_update_task` - PUT /tasks/{id}
- `amp_dispatch_task` - POST /tasks/{id}/dispatch
- `amp_complete_task` - POST /tasks/{id}/complete
- `amp_block_task` - POST /tasks/{id}/block
- `amp_list_ready_tasks` - GET /projects/{id}/ready-tasks

### Knowledge Base
- `amp_create_kb_entry` - POST /kb
- `amp_search_kb` - POST /kb/search
- `amp_get_kb_entry` - GET /kb/entries/{id}
- `amp_get_project_kb` - GET /projects/{id}/kb

### Dashboard & Context
- `amp_get_dashboard` - GET /projects/{id}/dashboard
- `amp_validate_context` - POST /context/validate

### Health
- `amp_health_check` - GET /health

## Dependencies

```go
require (
    github.com/gin-gonic/gin v1.9.1              // HTTP framework
    github.com/mark3labs/mcp-go v0.5.0          // MCP protocol
    github.com/kolo/xmlrpc v0.0.0-20220921171641-a4b6fa1dd06b  // XML-RPC client
    github.com/spf13/viper v1.18.2              // Config management
    github.com/sirupsen/logrus v1.9.3           // Logging
    github.com/stretchr/testify v1.9.0          // Testing
)
```

## Implementation Phases

### Phase 1: Foundation
1. Set up Go project structure
2. Create domain models
3. Define repository interfaces
4. Implement configuration

### Phase 2: Odoo Integration
1. Implement Odoo XML-RPC client
2. Create repository implementations
3. Build use cases
4. Add error handling

### Phase 3: MCP Protocol
1. Set up mark3labs/mcp-go
2. Implement MCP tools mapping
3. Add SSE transport
4. Create health check

### Phase 4: Docker & Integration
1. Create Go Dockerfile
2. Update docker-compose
3. Update OpenCode config
4. Update skill documentation

### Phase 5: Testing & Validation
1. Unit tests
2. Integration tests
3. End-to-end validation
4. Documentation update

## Migration Notes

### Data Models
All Python Pydantic models map to Go structs with tags for JSON/XML marshaling.

### Error Handling
- Python exceptions → Go errors with wrapping
- HTTP status codes preserved
- Structured error responses

### Configuration
- Environment variables remain the same
- Added support for config files (YAML/JSON)
- Validation on startup

### API Compatibility
The MCP protocol is stateless JSON-RPC over SSE, so tools will have same signature as before but different transport.

## OpenCode Integration

Update `opencode.json` to use the new Go MCP:
```json
{
  "mcp": {
    "amp-odoo": {
      "type": "remote",
      "url": "http://localhost:8000",
      "enabled": true
    }
  }
}
```

The Go server will expose SSE endpoint at `/mcp` which OpenCode will connect to.

## Skills Updates Required

Update skill documentation to reference new MCP tool names and explain the hexagonal architecture for future contributors.
