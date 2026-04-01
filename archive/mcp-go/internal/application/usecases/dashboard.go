package usecases

import (
	"context"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// DashboardUseCase handles dashboard-related business logic
type DashboardUseCase struct {
	repo repository.DashboardRepository
}

// NewDashboardUseCase creates a new dashboard use case
func NewDashboardUseCase(repo repository.DashboardRepository) *DashboardUseCase {
	return &DashboardUseCase{repo: repo}
}

func (uc *DashboardUseCase) GetProjectDashboard(ctx context.Context, projectID int) (*models.Dashboard, error) {
	return uc.repo.GetProjectDashboard(ctx, projectID)
}

func (uc *DashboardUseCase) ValidateContext(ctx context.Context, req models.ValidateContextRequest) (bool, string, error) {
	return uc.repo.ValidateContext(ctx, req)
}
