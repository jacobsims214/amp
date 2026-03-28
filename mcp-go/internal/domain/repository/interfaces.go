package repository

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
)

// ProjectRepository defines the interface for project operations
type ProjectRepository interface {
	Create(ctx context.Context, req models.CreateProjectRequest) (*models.Project, error)
	GetByID(ctx context.Context, id int) (*models.Project, error)
	GetByCode(ctx context.Context, code string) (*models.Project, error)
	List(ctx context.Context, limit, offset int) ([]models.Project, error)
	Update(ctx context.Context, req models.UpdateProjectRequest) error
}

// EpicRepository defines the interface for epic operations
type EpicRepository interface {
	Create(ctx context.Context, req models.CreateEpicRequest) (*models.Epic, error)
	GetByID(ctx context.Context, id int) (*models.Epic, error)
	ListByProject(ctx context.Context, projectID int) ([]models.Epic, error)
	UpdateDAG(ctx context.Context, epicID int, dagJSON string) error
}

// StoryRepository defines the interface for story operations
type StoryRepository interface {
	Create(ctx context.Context, req models.CreateStoryRequest) (*models.Story, error)
	GetByID(ctx context.Context, id int) (*models.Story, error)
	ListByEpic(ctx context.Context, epicID int) ([]models.Story, error)
}

// TaskRepository defines the interface for task operations
type TaskRepository interface {
	Create(ctx context.Context, req models.CreateTaskRequest) (*models.Task, error)
	GetByID(ctx context.Context, id int) (*models.Task, error)
	Update(ctx context.Context, req models.UpdateTaskRequest) error
	Dispatch(ctx context.Context, taskID int, agentID string) error
	Complete(ctx context.Context, taskID int) error
	Block(ctx context.Context, taskID int, reason string) error
	ListTasks(ctx context.Context, projectID, epicID, storyID int, state string) ([]models.Task, error)
	ListByStory(ctx context.Context, storyID int) ([]models.Task, error)
	ListByProject(ctx context.Context, projectID int, state string) ([]models.Task, error)
	ListReady(ctx context.Context, projectID int) ([]models.Task, error)
	AddComment(ctx context.Context, req models.AddCommentRequest) error
}

// KBRepository defines the interface for knowledge base operations
type KBRepository interface {
	Create(ctx context.Context, req models.CreateKBEntryRequest) (*models.KBEntry, error)
	GetByID(ctx context.Context, id int) (*models.KBEntry, error)
	Search(ctx context.Context, req models.SearchKBRequest) ([]models.KBEntry, error)
	ListByProject(ctx context.Context, projectID int, limit int) ([]models.KBEntry, error)
	ListByTask(ctx context.Context, taskID int) ([]models.KBEntry, error)
}

// DashboardRepository defines the interface for dashboard operations
type DashboardRepository interface {
	GetProjectDashboard(ctx context.Context, projectID int) (*models.Dashboard, error)
	ValidateContext(ctx context.Context, req models.ValidateContextRequest) (bool, string, error)
}
