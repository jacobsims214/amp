package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// The audit export's two formats (#2431):
//
//   - ndjson: one AuditEntry JSON object per line, exactly as /api/audit
//     and the webhooks send it (every v2 field, omitempty).
//   - csv: a header row, then one flat row per entry with every column;
//     details, before and after are JSON in their cells. A cell that a
//     spreadsheet would run as a formula (starting with = + - @, a tab or
//     a carriage return) is prefixed with ' (OWASP CSV injection): user
//     agents, summaries and emails can be chosen by whoever made the
//     request.
//
// Times are UTC RFC 3339 with microseconds.

// AuditCSVColumns is the export's CSV header, in order.
var AuditCSVColumns = []string{
	"id", "at", "schema_version", "action", "outcome", "reason", "summary",
	"actor_type", "actor_user_id", "actor_email", "on_behalf_of_user_id", "agent_profile",
	"auth_method", "via_token", "token_id", "session_id", "client", "source_ip", "user_agent", "request_id", "trace_id",
	"org_id", "project_id", "target_type", "target_id", "target_name",
	"details", "before", "after", "chain_key", "chain_seq", "hash",
}

// auditExporter writes the export to w. Nothing is sent until the first
// row (or finish), so a refusal is still an ordinary JSON error.
type auditExporter struct {
	w       http.ResponseWriter
	format  string
	name    string
	csv     *csv.Writer
	started bool
	rows    int
}

func newAuditExporter(w http.ResponseWriter, format string, f domain.AuditFilter) *auditExporter {
	scope := "all"
	switch {
	case f.ProjectID != 0:
		scope = "project-" + strconv.Itoa(f.ProjectID)
	case f.OrgID != 0:
		scope = "org-" + strconv.Itoa(f.OrgID)
	}
	ext := format
	if format == service.AuditExportNDJSON {
		ext = "ndjson"
	}
	return &auditExporter{w: w, format: format,
		name: fmt.Sprintf("amp-audit-%s-%s.%s", scope, time.Now().UTC().Format("20060102-150405"), ext)}
}

func (x *auditExporter) start() {
	if x.started {
		return
	}
	x.started = true
	h := x.w.Header()
	if x.format == service.AuditExportCSV {
		h.Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	}
	h.Set("Content-Disposition", `attachment; filename="`+x.name+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	x.w.WriteHeader(http.StatusOK)
	if x.format == service.AuditExportCSV {
		x.csv = csv.NewWriter(x.w)
		_ = x.csv.Write(AuditCSVColumns)
	}
}

// write emits one entry; every page's worth is flushed to the client.
func (x *auditExporter) write(e domain.AuditEntry) error {
	x.start()
	e.At = e.At.UTC()
	if x.format == service.AuditExportCSV {
		if err := x.csv.Write(auditCSVRow(e)); err != nil {
			return err
		}
	} else {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := x.w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	x.rows++
	if x.rows%service.AuditExportPage == 0 {
		return x.flush()
	}
	return nil
}

func (x *auditExporter) flush() error {
	if x.csv != nil {
		x.csv.Flush()
		if err := x.csv.Error(); err != nil {
			return err
		}
	}
	err := http.NewResponseController(x.w).Flush()
	if err == http.ErrNotSupported {
		return nil
	}
	return err
}

// finish completes a successful export (an empty one is a header only).
func (x *auditExporter) finish() {
	x.start()
	_ = x.flush()
}

func auditCSVRow(e domain.AuditEntry) []string {
	optInt := func(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	}
	js := func(m map[string]any) string {
		if m == nil {
			return ""
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	tokenID := ""
	if e.TokenID != nil {
		tokenID = strconv.FormatInt(*e.TokenID, 10)
	}
	chainSeq := ""
	if e.ChainSeq != 0 {
		chainSeq = strconv.FormatInt(e.ChainSeq, 10)
	}
	row := []string{
		strconv.FormatInt(e.ID, 10), e.At.Format("2006-01-02T15:04:05.000000Z07:00"), strconv.Itoa(e.SchemaVersion),
		e.Action, e.Outcome, e.Reason, e.Summary,
		e.ActorType, optInt(e.ActorUserID), e.ActorEmail, optInt(e.OnBehalfOfUserID), e.AgentProfile,
		e.AuthMethod, strconv.FormatBool(e.ViaToken), tokenID, e.SessionID, e.Client, e.SourceIP, e.UserAgent, e.RequestID, e.TraceID,
		optInt(e.OrgID), optInt(e.ProjectID), e.TargetType, e.TargetID, e.TargetName,
		js(e.Details), js(e.Before), js(e.After), e.ChainKey, chainSeq, e.Hash,
	}
	for i, v := range row {
		row[i] = csvSafe(v)
	}
	return row
}

// csvSafe keeps a spreadsheet from running a cell as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
