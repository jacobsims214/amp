package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// Server represents the HTTP server
type Server struct {
	mcpServer  *server.MCPServer
	port       int
	httpServer *http.Server
	sseServer  *server.SSEServer
}

// NewServer creates a new HTTP server
func NewServer(mcpServer *server.MCPServer, port int) *Server {
	s := &Server{
		mcpServer: mcpServer,
		port:      port,
	}

	// Create SSE server
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	s.sseServer = server.NewSSEServer(mcpServer, baseURL)

	return s
}

// Start starts the HTTP server
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)

	// Start SSE server in background
	go func() {
		if err := s.sseServer.Start(addr); err != nil {
			fmt.Printf("SSE server error: %v\n", err)
		}
	}()

	// Give SSE server time to start
	time.Sleep(100 * time.Millisecond)

	return nil
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.sseServer != nil {
		return s.sseServer.Shutdown(ctx)
	}
	return nil
}
