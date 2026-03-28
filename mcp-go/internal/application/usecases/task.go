package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// TaskUseCase handles task-related business logic
type TaskUseCase struct {
	repo repository.TaskRepository
}

// NewTaskUseCase creates a new task use case
func NewTaskUseCase(repo repository.TaskRepository) *TaskUseCase {
	return &TaskUseCase{repo: repo}
}

func (uc *TaskUseCase) Create(ctx context.Context, req models.CreateTaskRequest) (*models.Task, error) {
	return uc.repo.Create(ctx, req)
}

func (uc *TaskUseCase) GetByID(ctx context.Context, id int) (*models.Task, error) {
	return uc.repo.GetByID(ctx, id)
}

func (uc *TaskUseCase) Update(ctx context.Context, req models.UpdateTaskRequest) error {
	return uc.repo.Update(ctx, req)
}

func (uc *TaskUseCase) Dispatch(ctx context.Context, taskID int, agentID string) error {
	return uc.repo.Dispatch(ctx, taskID, agentID)
}

func (uc *TaskUseCase) Complete(ctx context.Context, taskID int) error {
	return uc.repo.Complete(ctx, taskID)
}

func (uc *TaskUseCase) Block(ctx context.Context, taskID int, reason string) error {
	return uc.repo.Block(ctx, taskID, reason)
}

func (uc *TaskUseCase) ListTasks(ctx context.Context, projectID, epicID, storyID int, state string) ([]models.Task, error) {
	return uc.repo.ListTasks(ctx, projectID, epicID, storyID, state)
}

func (uc *TaskUseCase) ListByStory(ctx context.Context, storyID int) ([]models.Task, error) {
	return uc.repo.ListByStory(ctx, storyID)
}

func (uc *TaskUseCase) ListByProject(ctx context.Context, projectID int, state string) ([]models.Task, error) {
	return uc.repo.ListByProject(ctx, projectID, state)
}

func (uc *TaskUseCase) ListReady(ctx context.Context, projectID int) ([]models.Task, error) {
	return uc.repo.ListReady(ctx, projectID)
}

func (uc *TaskUseCase) AddComment(ctx context.Context, req models.AddCommentRequest) error {
	return uc.repo.AddComment(ctx, req)
}
