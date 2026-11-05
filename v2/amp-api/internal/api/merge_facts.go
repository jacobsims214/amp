package api

import "github.com/simstech/amp-api/internal/service"

// SetMergeFacts wires the repository-facts cache the merge-alert policy reads
// (AMP #3158). The My work read path uses it for cached facts only: it starts a
// bounded background refresh at most, and never waits for one, so wiring it
// cannot slow the page or make it depend on GitHub being reachable.
func (h *RestHandler) SetMergeFacts(f *service.GitHubFactsCoordinator) { h.svc.Facts = f }

// SetMergeAlertPerson wires who a ticket's owner is to the merge policy (their
// GitHub login, CODEOWNERS ownership). Left unset AMP cannot attribute a rule
// that names someone, so such a rule stays the honest unknown arm rather than an
// alert addressed to the wrong person; see service.MergeAlertPersonSource.
func (h *RestHandler) SetMergeAlertPerson(src service.MergeAlertPersonSource) {
	h.svc.MergeAlertPerson = src
}
