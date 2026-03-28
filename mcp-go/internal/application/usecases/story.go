package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// StoryUseCase handles story-related business logic
type StoryUseCase struct {
	repo repository.StoryRepository
}

// NewStoryUseCase creates a new story use case
func NewStoryUseCase(repo repository.StoryRepository) *StoryUseCase {
	return &StoryUseCase{repo: repo}
}

func (uc *StoryUseCase) Create(ctx context.Context, req models.CreateStoryRequest) (*models.Story, error) {
	return uc.repo.Create(ctx, req)
}

func (uc *StoryUseCase) GetByID(ctx context.Context, id int) (*models.Story, error) {
	return uc.repo.GetByID(ctx, id)
}

func (uc *StoryUseCase) ListByEpic(ctx context.Context, epicID int) ([]models.Story, error) {
	return uc.repo.ListByEpic(ctx, epicID)
}
