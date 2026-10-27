package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

const backfillOwnersUsage = `usage: amp-api backfill-owners [-apply] [-project ID] [-actor EMAIL] [-json]

Sets owner_user_id = created_by on open (not completed), unowned tickets
created by a person (users.kind = 'human', not deactivated); tickets created
by agent accounts are left alone. "Mine" means owner_user_id = me, so this keeps
those tickets in their creator's My work (KB decisions/human-work-ux.md §1).

Dry run by default: prints how many tickets each project would change and
changes nothing. -apply changes them in one transaction and writes one audit
entry per project (action task.owner.backfill, with the ticket ids). Running
it again finds nothing left to change.

  -apply         make the change (default: dry run)
  -project ID    one project only (default: every project)
  -actor EMAIL   who ran it, recorded on the audit entries (default: amp-api backfill-owners)
  -json          print the result as JSON

Reads DATABASE_URL. It doesn't migrate: run it against a database amp-api has
already migrated.
`

// runBackfillOwnersCLI runs `amp-api backfill-owners <args>` and returns the
// exit code: 0 done (or nothing to do), 2 usage or runtime error.
func runBackfillOwnersCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backfill-owners", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, backfillOwnersUsage) }
	apply := fs.Bool("apply", false, "make the change (default: dry run)")
	project := fs.Int("project", 0, "one project only")
	actor := fs.String("actor", "amp-api backfill-owners", "who ran it, for the audit entries")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *project < 0 {
		if err == nil {
			fmt.Fprint(stderr, backfillOwnersUsage)
		}
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	repo, err := cliRepo(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer repo.Close()

	var groups []repository.OwnerBackfillProject
	if *apply {
		groups, err = repo.ApplyOwnerBackfill(ctx, *project, domain.AuditEntry{
			ActorEmail: *actor,
			ActorType:  domain.AuditActorSystem,
			Client:     domain.AuditClientCLI,
		})
	} else {
		groups, err = repo.OwnerBackfillCandidates(ctx, *project)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	total := 0
	for _, g := range groups {
		total += len(g.TaskIDs)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if groups == nil {
			groups = []repository.OwnerBackfillProject{}
		}
		_ = enc.Encode(map[string]any{"applied": *apply, "total": total, "projects": groups})
		return 0
	}
	mode, verb := "DRY RUN (nothing changed; -apply to change)", "would set"
	if *apply {
		mode, verb = "APPLIED", "set"
	}
	fmt.Fprintf(stdout, "backfill-owners: %s\n", mode)
	for _, g := range groups {
		fmt.Fprintf(stdout, "  project %d %q: %s the owner on %d ticket(s)\n", g.ProjectID, g.ProjectName, verb, len(g.TaskIDs))
	}
	fmt.Fprintf(stdout, "total: %d ticket(s) in %d project(s)\n", total, len(groups))
	return 0
}
