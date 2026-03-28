package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// KBUseCase handles knowledge base-related business logic
type KBUseCase struct {
	repo repository.KBRepository
}

// NewKBUseCase creates a new KB use case
func NewKBUseCase(repo repository.KBRepository) *KBUseCase {
	return &KBUseCase{repo: repo}
}

func (uc *KBUseCase) Create(ctx context.Context, req models.CreateKBEntryRequest) (*models.KBEntry, error) {
	return uc.repo.Create(ctx, req)
}

func (uc *KBUseCase) GetByID(ctx context.Context, id int) (*models.KBEntry, error) {
	return uc.repo.GetByID(ctx, id)
}

func (uc *KBUseCase) Search(ctx context.Context, req models.SearchKBRequest) ([]models.KBEntry, error) {
	return uc.repo.Search(ctx, req)
}

func (uc *KBUseCase) ListByProject(ctx context.Context, projectID int, limit int) ([]models.KBEntry, error) {
	return uc.repo.ListByProject(ctx, projectID, limit)
}

func (uc *KBUseCase) ListByTask(ctx context.Context, taskID int) ([]models.KBEntry, error) {
	return uc.repo.ListByTask(ctx, taskID)
}
