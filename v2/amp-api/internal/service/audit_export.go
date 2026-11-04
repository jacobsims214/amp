package service

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Audit log filters and export (#2431, KB decisions/audit-log-v2.md
// section 4). REST /api/audit, /api/audit/export and MCP amp_audit_log
// read the same filter, with the same names, through ParseAuditFilter.

// AuditExportPage is how many rows the export reads per query. The export
// pages by id (keyset), so it has no row cap and holds one page at a time.
const AuditExportPage = 500

// Audit export formats.
const (
	AuditExportCSV    = "csv"
	AuditExportNDJSON = "ndjson"
)

// ParseAuditFilter reads an audit filter from named values (a URL query,
// or MCP arguments as strings): org_id, project_id, actor_id, action,
// before (or before_id), limit, actor (email substring), actor_type,
// target_type, target_id, ip (address or CIDR), outcome, client, from and
// to (RFC 3339, or a date: from that day's start, to the end of that day,
// UTC). An unknown value is a 400, never a silently wider result.
func ParseAuditFilter(get func(string) string) (domain.AuditFilter, error) {
	var f domain.AuditFilter
	var err error
	num := func(k string) int {
		v := strings.TrimSpace(get(k))
		if v == "" || err != nil {
			return 0
		}
		n, e := strconv.Atoi(v)
		if e != nil {
			err = authz.BadRequest("%s must be a whole number", k)
		}
		return max(n, 0) // a negative number means unset, as it always has
	}
	f.OrgID, f.ProjectID, f.ActorUserID, f.Limit = num("org_id"), num("project_id"), num("actor_id"), num("limit")
	before := num("before")
	if before == 0 {
		before = num("before_id")
	}
	f.BeforeID = int64(before)
	if err != nil {
		return f, err
	}
	f.Action = strings.TrimSpace(get("action"))
	f.Actor = strings.TrimSpace(get("actor"))
	f.TargetType = strings.TrimSpace(get("target_type"))
	f.TargetID = strings.TrimSpace(get("target_id"))
	if len(f.Actor) > 320 || len(f.Action) > 200 || len(f.TargetType) > 100 || len(f.TargetID) > 200 {
		return f, authz.BadRequest("a filter value is too long")
	}
	f.ActorType = strings.TrimSpace(get("actor_type"))
	switch f.ActorType {
	case "", domain.AuditActorHuman, domain.AuditActorAgent, domain.AuditActorSystem, domain.AuditActorAnonymous:
	default:
		return f, authz.BadRequest("actor_type must be human, agent, system or anonymous")
	}
	f.Outcome = strings.TrimSpace(get("outcome"))
	switch f.Outcome {
	case "", domain.AuditOutcomeSuccess, domain.AuditOutcomeDenied, domain.AuditOutcomeFailed:
	default:
		return f, authz.BadRequest("outcome must be success, denied or failed")
	}
	f.Client = strings.TrimSpace(get("client"))
	switch f.Client {
	case "", domain.AuditClientWeb, domain.AuditClientMCP, domain.AuditClientHarness, domain.AuditClientAPI,
		domain.AuditClientRelay, domain.AuditClientCLI, domain.AuditClientSystem:
	default:
		return f, authz.BadRequest("client must be web, mcp, harness, api, relay, cli or system")
	}
	if ip := strings.TrimSpace(get("ip")); ip != "" {
		if pfx, e := netip.ParsePrefix(ip); e == nil {
			f.IP = pfx.Masked().String()
		} else if a, e := netip.ParseAddr(ip); e == nil {
			f.IP = a.Unmap().String()
		} else {
			return f, authz.BadRequest("ip must be an address (203.0.113.7) or a CIDR prefix (203.0.113.0/24)")
		}
	}
	if f.From, err = parseAuditTime(get("from"), false); err != nil {
		return f, authz.BadRequest("from: %s", err.Error())
	}
	if f.To, err = parseAuditTime(get("to"), true); err != nil {
		return f, authz.BadRequest("to: %s", err.Error())
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return f, authz.BadRequest("from must be before to")
	}
	return f, nil
}

// parseAuditTime reads RFC 3339 or a date. A date as an end (to) means the
// end of that day, since to is exclusive.
func parseAuditTime(v string, end bool) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return &t, nil
	}
	if d, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			d = d.AddDate(0, 0, 1)
		}
		return &d, nil
	}
	return nil, fmt.Errorf("use RFC 3339 (2026-10-26T14:00:00Z) or a date (2026-10-26)")
}

// ExportAudit streams every entry matching f that p may read to emit,
// newest first, reading a page at a time by id, and records the export as
// audit.export (with the filter and how many rows were emitted, also when
// emit failed part way). f.BeforeID, if set, is where the export starts;
// f.Limit is ignored. The export itself is not in its own output.
func (s *Service) ExportAudit(ctx context.Context, p authz.Principal, f domain.AuditFilter, format string,
	emit func(domain.AuditEntry) error) (int, error) {
	switch format {
	case AuditExportCSV, AuditExportNDJSON:
	default:
		return 0, authz.BadRequest("format must be csv or ndjson")
	}
	if err := s.authorizeAudit(ctx, p, f); err != nil {
		return 0, err
	}
	rows := 0
	var err error
	page := f
	page.Limit = AuditExportPage
	for {
		var entries []domain.AuditEntry
		entries, err = s.Repo.ListAudit(ctx, page)
		if err != nil {
			break
		}
		for _, e := range entries {
			if err = emit(e); err != nil {
				break
			}
			rows++
		}
		if err != nil || len(entries) < AuditExportPage {
			break
		}
		page.BeforeID = entries[len(entries)-1].ID
	}
	f.Limit = 0
	s.RecordAuditExport(context.WithoutCancel(ctx), p, f, format, rows)
	return rows, err
}
