package mcp

import (
	"github.com/simstech/amp-api/internal/service"
)

// SetMergeFacts wires the repository-facts cache the merge-alert policy reads
// (AMP #3158). amp_list_tasks' waiting_on_people is fed by the same
// Service.WaitingOn the REST My work tab uses, so this is the only place MCP
// needs: an item's alert arm is already on it.
func (s *Server) SetMergeFacts(f *service.GitHubFactsCoordinator) { s.svc.Facts = f }
