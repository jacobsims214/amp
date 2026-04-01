package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// EpicUseCase handles epic-related business logic
type EpicUseCase struct {
	repo repository.EpicRepository
}

// NewEpicUseCase creates a new epic use case
func NewEpicUseCase(repo repository.EpicRepository) *EpicUseCase {
	return &EpicUseCase{repo: repo}
}

func (uc *EpicUseCase) Create(ctx context.Context, req models.CreateEpicRequest) (*models.Epic, error) {
	return uc.repo.Create(ctx, req)
}

func (uc *EpicUseCase) GetByID(ctx context.Context, id int) (*models.Epic, error) {
	return uc.repo.GetByID(ctx, id)
}

func (uc *EpicUseCase) ListByProject(ctx context.Context, projectID int) ([]models.Epic, error) {
	return uc.repo.ListByProject(ctx, projectID)
}


func (uc *EpicUseCase) SetState(ctx context.Context, epicID int, state string, reason string) error {
	return uc.repo.SetState(ctx, epicID, state, reason)
}

func (uc *EpicUseCase) Delete(ctx context.Context, epicID int) error {
	return uc.repo.Delete(ctx, epicID)
}

func (uc *EpicUseCase) DeleteByProject(ctx context.Context, projectID int) (int, error) {
	return uc.repo.DeleteByProject(ctx, projectID)
}
