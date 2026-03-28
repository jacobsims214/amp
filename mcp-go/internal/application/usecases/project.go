package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// ProjectUseCase handles project-related business logic
type ProjectUseCase struct {
	repo repository.ProjectRepository
}

// NewProjectUseCase creates a new project use case
func NewProjectUseCase(repo repository.ProjectRepository) *ProjectUseCase {
	return &ProjectUseCase{repo: repo}
}

func (uc *ProjectUseCase) Create(ctx context.Context, req models.CreateProjectRequest) (*models.Project, error) {
	return uc.repo.Create(ctx, req)
}

func (uc *ProjectUseCase) GetByID(ctx context.Context, id int) (*models.Project, error) {
	return uc.repo.GetByID(ctx, id)
}

func (uc *ProjectUseCase) GetByCode(ctx context.Context, code string) (*models.Project, error) {
	return uc.repo.GetByCode(ctx, code)
}

func (uc *ProjectUseCase) List(ctx context.Context, limit, offset int) ([]models.Project, error) {
	return uc.repo.List(ctx, limit, offset)
}

func (uc *ProjectUseCase) Update(ctx context.Context, req models.UpdateProjectRequest) error {
	return uc.repo.Update(ctx, req)
}
