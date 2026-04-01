package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/amp/mcp-go/internal/application/mcp"
	"github.com/amp/mcp-go/internal/application/usecases"
	"github.com/amp/mcp-go/internal/infrastructure/config"
	"github.com/amp/mcp-go/internal/infrastructure/odoo"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create Odoo client
	odooClient, err := odoo.NewClient(
		cfg.Odoo.URL,
		cfg.Odoo.DB,
		cfg.Odoo.Username,
		cfg.Odoo.Password,
	)
	if err != nil {
		log.Fatalf("Failed to connect to Odoo: %v", err)
	}
	log.Println("Connected to Odoo successfully")

	// Create repositories
	projectRepo := odoo.NewProjectRepository(odooClient)
	epicRepo := odoo.NewEpicRepository(odooClient)
	storyRepo := odoo.NewStoryRepository(odooClient)
	taskRepo := odoo.NewTaskRepository(odooClient)
	kbRepo := odoo.NewKBRepository(odooClient)
	dashboardRepo := odoo.NewDashboardRepository(odooClient)

	// Create use cases
	projectUC := usecases.NewProjectUseCase(projectRepo)
	epicUC := usecases.NewEpicUseCase(epicRepo)
	storyUC := usecases.NewStoryUseCase(storyRepo)
	taskUC := usecases.NewTaskUseCase(taskRepo)
	kbUC := usecases.NewKBUseCase(kbRepo)
	dashboardUC := usecases.NewDashboardUseCase(dashboardRepo)

	// Create MCP server
	mcpServer := server.NewMCPServer(
		"amp-odoo-mcp",
		"2.0.0",
		server.WithToolCapabilities(true),
	)

	// Create MCP handler and register tools
	handler := mcp.NewHandler(
		projectUC,
		epicUC,
		storyUC,
		taskUC,
		kbUC,
		dashboardUC,
		odooClient,
	)
	handler.RegisterTools(mcpServer)

	// Create SSE server directly
	baseURL := fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	sseServer := server.NewSSEServer(mcpServer, baseURL)

	// Start SSE server in a goroutine
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Server.Port)
		log.Printf("Starting MCP SSE server on port %d", cfg.Server.Port)
		if err := sseServer.Start(addr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx := context.Background()
	if err := sseServer.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	log.Println("Server stopped")
}
