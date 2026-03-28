# AMP MCP Server - Go Implementation

Go-based MCP (Model Context Protocol) server for AMP Odoo Project Management integration.

## Architecture

This implementation uses **Hexagonal Architecture** (Ports & Adapters pattern):

```
┌─────────────────────────────────────────────────────────────┐
│                    Infrastructure Layer                      │
│         (Odoo XML-RPC, HTTP Server, MCP SSE)                │
└─────────────────────────────────────────────────────────────┘
                              │
┌─────────────────────────────┼───────────────────────────────┐
│                   Application Layer                          │
│              (Use Cases, MCP Handlers)                       │
└─────────────────────────────┼───────────────────────────────┘
                              │
┌─────────────────────────────┼───────────────────────────────┐
│                      Domain Layer                            │
│              (Models, Repository Interfaces)                 │
└─────────────────────────────────────────────────────────────┘
```

## Project Structure

```
mcp-go/
├── cmd/server/          # Application entry point
├── internal/
│   ├── domain/          # Business logic and interfaces
│   │   ├── models/      # Domain entities
│   │   └── repository/  # Repository interfaces (ports)
│   ├── application/     # Use cases and MCP protocol
│   │   ├── usecases/    # Business logic orchestration
│   │   └── mcp/         # MCP protocol handlers
│   └── infrastructure/  # External adapters
│       ├── odoo/        # Odoo XML-RPC client
│       ├── config/      # Configuration management
│       └── http/        # HTTP server with SSE
└── Dockerfile
```

## Features

All features from the Python implementation ported to Go:

### Projects
- Create, list, get, update projects
- Find project by code

### Epics
- Create, get, list epics
- Update DAG structure

### Stories
- Create, get, list stories

### Tasks
- Create, get, update tasks
- Dispatch, complete, block tasks
- List ready tasks for dispatch
- Add task comments

### Knowledge Base
- Create, search KB entries
- Get project and task KB entries

### Dashboard & Context
- Get project dashboard
- Validate .amp.json context

## Environment Variables

```bash
ODOO_URL=http://host.docker.internal:8069
ODOO_DB=odoo19
ODOO_USER=admin
ODOO_PASSWORD=admin
MCP_PORT=8000
```

## Running

### Local Development
```bash
cd mcp-go
go mod tidy
go run cmd/server/main.go
```

### Docker
```bash
docker-compose up --build
```

## MCP Protocol

The server exposes MCP tools via SSE at `http://localhost:8000/mcp`.

All tools are prefixed with `amp_`:
- `amp_create_project`
- `amp_list_projects`
- `amp_create_epic`
- `amp_create_story`
- `amp_create_task`
- `amp_dispatch_task`
- `amp_complete_task`
- And more...

## Dependencies

- `mark3labs/mcp-go` - MCP protocol implementation
- `kolo/xmlrpc` - XML-RPC client for Odoo
- `gin-gonic/gin` - HTTP framework
- `spf13/viper` - Configuration management

## Testing

```bash
go test ./...
```

## Migration from Python

This Go implementation is a complete port of the Python FastAPI server with the following improvements:
- Proper MCP protocol support via SSE
- Type-safe implementation
- Better error handling
- Hexagonal architecture for maintainability
- Improved performance
