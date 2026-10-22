package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// The plan-approval dispatch gate fails closed: when the project can't be
// loaded, the dispatch is refused (retryable 503), never let through
// unchecked. The approve/dispatch flow itself is covered end to end by
// cmd/server TestPlanApproval.
func TestPlanApprovalGate(t *testing.T) {
	approved := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	before := &domain.Task{ID: 7, ProjectID: 3, CreatedAt: approved.Add(-time.Hour)}
	after := &domain.Task{ID: 8, ProjectID: 3, CreatedAt: approved.Add(time.Hour)}

	tests := []struct {
		name       string
		proj       *domain.Project
		lookupErr  error
		task       *domain.Task
		wantStatus int // 0: allowed
		wantMsg    string
	}{
		{name: "lookup fails, setting unknown: refused", lookupErr: errors.New("conn reset"), task: before, wantStatus: 503, wantMsg: "try the dispatch again"},
		{name: "no project and no error: refused", task: before, wantStatus: 503, wantMsg: "project not found"},
		{name: "setting off", proj: &domain.Project{}, task: after},
		{name: "setting on, never approved", proj: &domain.Project{RequirePlanApproval: true}, task: before, wantStatus: 403, wantMsg: "approved"},
		{name: "setting on, planned before approval", proj: &domain.Project{RequirePlanApproval: true, PlanApprovedAt: &approved}, task: before},
		{name: "setting on, planned after approval", proj: &domain.Project{RequirePlanApproval: true, PlanApprovedAt: &approved}, task: after, wantStatus: 403, wantMsg: "approved"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := planApprovalGate(tc.proj, tc.lookupErr, tc.task, "/project/1/plan")
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("want allowed, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want %d, dispatch was allowed", tc.wantStatus)
			}
			if got := authz.StatusOf(err, 500); got != tc.wantStatus {
				t.Errorf("status = %d, want %d (%v)", got, tc.wantStatus, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q lacks %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
