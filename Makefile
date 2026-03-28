.PHONY: help up down logs status test

help:
	@echo "AMP MCP Server Commands:"
	@echo "  make up      - Start MCP server"
	@echo "  make down    - Stop MCP server"
	@echo "  make logs    - View logs"
	@echo "  make status  - Check health"
	@echo "  make test    - Test Odoo connection"

up:
	docker-compose up -d

down:
	docker-compose down

logs:
	docker-compose logs -f

status:
	@curl -s -N http://localhost:8000/sse --max-time 2 | head -c 200 || echo "SSE endpoint reachable"

test:
	@echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | curl -s -X POST http://localhost:8000/message -H "Content-Type: application/json" -d @-
