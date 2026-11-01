package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/auditchain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// loadAuditKeys reads the checkpoint signing key (AUDIT_SIGNING_KEY or
// AUDIT_SIGNING_KEY_FILE, from a Kubernetes Secret) and any retired public
// keys. A missing or unreadable key never stops amp-api: checkpoints are
// then written unsigned, and verify says so. The key itself is never
// logged, only its id.
func loadAuditKeys() *auditchain.Keyring {
	keys, err := auditchain.LoadKeyring(os.Getenv)
	if err != nil {
		slog.Error("audit signing key not usable; audit checkpoints will be unsigned", "err", err)
	}
	if keys.CanSign() {
		slog.Info("audit checkpoints are signed", "key_id", keys.SignerID())
	} else if err == nil {
		slog.Warn("AUDIT_SIGNING_KEY not set: audit checkpoints are unsigned, so tampering by someone with database access can't be proven (see `amp-api audit keygen`)")
	}
	return keys
}

// auditRetentionFromEnv is AUDIT_RETENTION_DAYS: unset means 365 days;
// "0" or "off" keeps entries forever; anything unparseable falls back to the
// default, with a warning.
func auditRetentionFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("AUDIT_RETENTION_DAYS"))
	switch v {
	case "":
		return service.DefaultAuditRetention
	case "0", "off":
		slog.Info("audit retention disabled (AUDIT_RETENTION_DAYS): audit entries are kept forever")
		return 0
	}
	d, err := strconv.Atoi(v)
	if err != nil || d < 0 {
		slog.Warn("bad AUDIT_RETENTION_DAYS; using the default", "value", v, "default_days", int(service.DefaultAuditRetention/(24*time.Hour)))
		return service.DefaultAuditRetention
	}
	r := time.Duration(d) * 24 * time.Hour
	if r < service.MinAuditRetention {
		// audit_purge() refuses younger rows anyway (migration 035).
		slog.Warn("AUDIT_RETENTION_DAYS is below the minimum; using the minimum", "value", v, "min_days", int(service.MinAuditRetention/(24*time.Hour)))
		return service.MinAuditRetention
	}
	return r
}

// purgeRoleSQL is migration 035's "audit_purge role setup" block: what a
// superuser runs once in AMP's database so retention can delete (#2439 H1).
func purgeRoleSQL() (string, error) {
	b, err := fs.ReadFile(migrationFS, "migrations/035_audit_integrity_review.sql")
	if err != nil {
		return "", err
	}
	const begin, end = "-- BEGIN audit_purge role setup\n", "-- END audit_purge role setup\n"
	s := string(b)
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		return "", errors.New("migration 035 has no audit_purge role setup block")
	}
	return s[i : j+len(end)], nil
}

const auditUsage = `usage: amp-api audit <command>

  verify [-org ID] [-json]   recompute the audit hash chains (every chain, or one org's)
                             and check the signed checkpoints; exits 1 if anything is wrong,
                             3 if nothing is wrong but nothing signed proves it (unproven)
  checkpoint                 sign every chain's head now (the server does this hourly)
  keygen -out FILE           make a checkpoint signing key: writes the private key to FILE
                             (mode 0600) and prints only the public key and its id
  purge-role-sql             print the SQL a superuser runs once so retention can delete
                             (the amp_audit_purge role); retention is off until then

verify and checkpoint read DATABASE_URL, and AUDIT_SIGNING_KEY (or AUDIT_SIGNING_KEY_FILE),
AUDIT_SIGNING_PUBLIC_KEYS, AUDIT_SIGNING_SINCE and AUDIT_RETENTION_DAYS like the server.
`

// runAuditCLI runs `amp-api audit <args>` and returns the exit code: 0 fine,
// 1 verification found problems, 2 usage or runtime error, 3 verification
// found no problems but nothing signed proves the chains (unproven).
func runAuditCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, auditUsage)
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	switch args[0] {
	case "purge-role-sql":
		sql, err := purgeRoleSQL()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprint(stdout, sql)
		return 0
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
		fs.SetOutput(stderr)
		out := fs.String("out", "", "file to write the private key to (must not exist)")
		if err := fs.Parse(args[1:]); err != nil || *out == "" {
			fmt.Fprint(stderr, auditUsage)
			return 2
		}
		return auditKeygen(*out, stdout, stderr)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		fs.SetOutput(stderr)
		org := fs.Int("org", 0, "verify this org's chain only")
		asJSON := fs.Bool("json", false, "print the report as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		repo, err := cliRepo(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer repo.Close()
		chains := []string{fmt.Sprintf("org:%d", *org)}
		if *org == 0 {
			if chains, err = repo.AuditChainKeys(ctx); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		}
		report, err := service.VerifyAuditChains(ctx, repo, loadAuditKeys(), auditchain.Policy{Retention: auditRetentionFromEnv()}, chains)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(report)
		} else {
			printReport(stdout, report)
		}
		switch report.Status {
		case auditchain.StatusOK:
			return 0
		case auditchain.StatusUnproven:
			return 3
		}
		return 1
	case "checkpoint":
		repo, err := cliRepo(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer repo.Close()
		service.AuditKeys = loadAuditKeys()
		n, err := service.New(repo, nil, nil).WriteAuditCheckpoints(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintf(stdout, "%d checkpoint(s) written (signed: %v)\n", n, service.AuditKeys.CanSign())
		return 0
	}
	fmt.Fprint(stderr, auditUsage)
	return 2
}

// cliRepo connects to DATABASE_URL. It doesn't migrate: the CLI only reads,
// or writes checkpoints into a database the server already migrated.
func cliRepo(ctx context.Context) (*repository.Repo, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	return repository.New(ctx, dsn)
}

func auditKeygen(path string, stdout, stderr io.Writer) int {
	seed, pub, id, err := auditchain.GenerateKey()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "keygen:", err)
		return 2
	}
	if _, err := f.WriteString(seed); err != nil {
		f.Close()
		fmt.Fprintln(stderr, "keygen:", err)
		return 2
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(stderr, "keygen:", err)
		return 2
	}
	fmt.Fprintf(stdout, "private key written to %s (keep it out of git and out of the database)\n", path)
	fmt.Fprintf(stdout, "key id:     %s\npublic key: %s\n\n", id, pub)
	fmt.Fprintf(stdout, "Store it as a Secret and point the chart at it, e.g.\n"+
		"  kubectl -n amp create secret generic amp-audit-signing --from-file=signing-key=%s\n"+
		"  helm upgrade ... --set ampApi.audit.signingKey.existingSecret=amp-audit-signing\n"+
		"then delete %s. Keep the public key: after a rotation, verify needs it in AUDIT_SIGNING_PUBLIC_KEYS.\n", path, path)
	return 0
}

func printReport(w io.Writer, r auditchain.Report) {
	signing := "unsigned (no key configured)"
	if r.Signing.Configured {
		signing = "key " + r.Signing.KeyID
	}
	fmt.Fprintf(w, "audit verify: %d chain(s), signing %s\n", len(r.Chains), signing)
	for _, c := range r.Chains {
		status := "OK"
		switch c.Status {
		case auditchain.StatusFailed:
			status = fmt.Sprintf("FAILED (%d problem(s))", len(c.Problems))
		case auditchain.StatusUnproven:
			status = "UNPROVEN"
		}
		fmt.Fprintf(w, "  %-12s %s: rows %d (seq %d..%d), head %d, anchor %d, checkpoints matched %d, signed %d\n",
			c.ChainKey, status, c.Rows, c.FirstSeq, c.LastSeq, c.HeadSeq, c.AnchorSeq, c.CheckpointsMatched, c.SignedCheckpoints)
		for _, p := range c.Problems {
			fmt.Fprintf(w, "    - %s seq=%d audit_id=%d checkpoint_id=%d: %s\n", p.Kind, p.Seq, p.AuditID, p.CheckpointID, p.Detail)
		}
		for _, wn := range c.Warnings {
			fmt.Fprintf(w, "    ! %s\n", wn)
		}
	}
	switch r.Status {
	case auditchain.StatusOK:
		fmt.Fprintln(w, "result: OK")
	case auditchain.StatusUnproven:
		fmt.Fprintln(w, "result: UNPROVEN (no problems found, but no valid signature vouches for every chain)")
	default:
		fmt.Fprintln(w, "result: TAMPERING OR LOSS DETECTED")
	}
}
