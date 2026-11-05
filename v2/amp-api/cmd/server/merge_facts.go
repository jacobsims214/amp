package main

import (
	"log/slog"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// githubFactsCoordinator builds the repository-facts cache coordinator the
// merge-alert policy reads (AMP #3158, #3156's coordinator).
//
// What it deliberately does NOT do is fail startup when GitHub is unreachable
// or a project has no credential: a missing fact is the classifier's unknown
// arm, which is the honest floor and the behavior before this existed. Only a
// wiring mistake — a nil store or reader factory, which cannot happen here —
// would be fatal.
//
// The coordinator reads the cache on the My work path and refreshes in the
// background, so nothing on a page waits for GitHub. It uses the project's
// sealed credential (#3154/#3155): the token reaches a request header in the
// reader and nowhere else.
func githubFactsCoordinator(repo *repository.Repo) *service.GitHubFactsCoordinator {
	if repo == nil {
		return nil
	}
	c, err := service.NewGitHubFactsCoordinator(service.GitHubFactsConfig{
		Store:       repo,
		Credentials: service.RepoGitHubFactCredentials{Repo: repo},
		Readers:     service.GitHubClientReaderFactory(githubfacts.Options{}),
	})
	if err != nil {
		// A nil store/credentials/reader factory is the only way here, and it is
		// a programming error rather than a deployment one; a nil coordinator
		// lands every merge item on the unknown arm, which is safe, so serve
		// rather than crash.
		slog.Error("repository facts coordinator not wired: merge items will classify as unknown", "err", err)
		return nil
	}
	return c
}
