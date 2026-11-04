package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ownerFixture is a scratch database (never a real one) with tickets that
// cover every case the owner backfill must tell apart.
type ownerFixture struct {
	d                   *integrityDB
	org, p1, p2         int
	person, agent, gone int
	want                map[int]int // ticket -> owner after apply
	untouched           map[int]*int
	p1Want, p2Want      []int
}

func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	d := newIntegrityDB(t)
	f := &ownerFixture{d: d, want: map[int]int{}, untouched: map[int]*int{}}
	q := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := d.db.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`INSERT INTO orgs (name, slug) VALUES ('Backfill org', 'backfill-org') RETURNING id`, &f.org)
	q(`INSERT INTO users (subject, email) VALUES ('s-person', 'person@x') RETURNING id`, &f.person)
	q(`INSERT INTO users (subject, email, kind) VALUES ('s-agent', 'agent@x', 'agent') RETURNING id`, &f.agent)
	q(`INSERT INTO users (subject, email, disabled_at) VALUES ('s-gone', 'gone@x', NOW()) RETURNING id`, &f.gone)
	var other int
	q(`INSERT INTO users (subject, email) VALUES ('s-other', 'other@x') RETURNING id`, &other)
	q(`INSERT INTO projects (name, code, org_id) VALUES ('Backfill P1', 'bp1', $1) RETURNING id`, &f.p1, f.org)
	q(`INSERT INTO projects (name, code, org_id) VALUES ('Backfill P2', 'bp2', $1) RETURNING id`, &f.p2, f.org)

	task := func(project int, state string, createdBy, owner *int) int {
		t.Helper()
		var epic, story, id int
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, project)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, project, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state, created_by, owner_user_id)
			VALUES ($1, $2, $3, 't', $4, $5, $6) RETURNING id`, &id, project, epic, story, state, createdBy, owner)
		return id
	}
	ptr := func(v int) *int { return &v }

	// Changed: open, unowned, created by an active person.
	for _, c := range []struct {
		project int
		state   string
	}{
		{f.p1, "backlog"}, {f.p1, "in_progress"}, {f.p1, "blocked"}, {f.p2, "backlog"},
	} {
		id := task(c.project, c.state, ptr(f.person), nil)
		f.want[id] = f.person
		if c.project == f.p1 {
			f.p1Want = append(f.p1Want, id)
		} else {
			f.p2Want = append(f.p2Want, id)
		}
	}
	// Left alone.
	f.untouched[task(f.p1, "completed", ptr(f.person), nil)] = nil             // done
	f.untouched[task(f.p1, "backlog", ptr(f.person), ptr(other))] = ptr(other) // already owned
	f.untouched[task(f.p1, "backlog", ptr(f.agent), nil)] = nil                // an agent made it
	f.untouched[task(f.p1, "backlog", ptr(f.gone), nil)] = nil                 // creator deactivated
	f.untouched[task(f.p2, "backlog", nil, nil)] = nil                         // no creator
	return f
}

func (f *ownerFixture) owner(t *testing.T, task int) *int {
	t.Helper()
	var o *int
	if err := f.d.db.QueryRow(context.Background(), `SELECT owner_user_id FROM tasks WHERE id = $1`, task).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

func (f *ownerFixture) auditRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.d.db.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'task.owner.backfill'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func runBackfill(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runBackfillOwnersCLI(args, &out, &errOut)
	if code != 0 {
		t.Logf("stderr: %s", errOut.String())
	}
	return out.String(), code
}

type backfillJSON struct {
	Applied  bool `json:"applied"`
	Total    int  `json:"total"`
	Projects []struct {
		ProjectID   int    `json:"project_id"`
		ProjectName string `json:"project_name"`
		TaskIDs     []int  `json:"task_ids"`
	} `json:"projects"`
}

func decodeBackfill(t *testing.T, s string) backfillJSON {
	t.Helper()
	var r backfillJSON
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return r
}

func TestBackfillOwners_DryRunChangesNothing(t *testing.T) {
	f := newOwnerFixture(t)
	t.Setenv("DATABASE_URL", f.d.dsn)

	out, code := runBackfill(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"DRY RUN", `project ` + strconv.Itoa(f.p1) + ` "Backfill P1": would set the owner on 3 ticket(s)`,
		`project ` + strconv.Itoa(f.p2) + ` "Backfill P2": would set the owner on 1 ticket(s)`, "total: 4 ticket(s) in 2 project(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}

	js, _ := runBackfill(t, "-json")
	r := decodeBackfill(t, js)
	if r.Applied || r.Total != 4 || len(r.Projects) != 2 || !slices.Equal(r.Projects[0].TaskIDs, f.p1Want) || !slices.Equal(r.Projects[1].TaskIDs, f.p2Want) {
		t.Fatalf("dry run JSON: %+v (want p1 %v, p2 %v)", r, f.p1Want, f.p2Want)
	}

	js, _ = runBackfill(t, "-json", "-project", strconv.Itoa(f.p2))
	if r := decodeBackfill(t, js); r.Total != 1 || len(r.Projects) != 1 || r.Projects[0].ProjectID != f.p2 {
		t.Fatalf("dry run -project: %+v", r)
	}

	for id := range f.want {
		if o := f.owner(t, id); o != nil {
			t.Errorf("dry run set the owner of #%d to %d", id, *o)
		}
	}
	if n := f.auditRows(t); n != 0 {
		t.Errorf("dry run wrote %d audit row(s)", n)
	}
}

func TestBackfillOwners_Apply(t *testing.T) {
	f := newOwnerFixture(t)
	t.Setenv("DATABASE_URL", f.d.dsn)

	// One project first: only its tickets change.
	js, code := runBackfill(t, "-apply", "-json", "-project", strconv.Itoa(f.p2), "-actor", "ops@x")
	if r := decodeBackfill(t, js); code != 0 || !r.Applied || r.Total != 1 || !slices.Equal(r.Projects[0].TaskIDs, f.p2Want) {
		t.Fatalf("apply -project: exit %d %+v", code, r)
	}
	for _, id := range f.p1Want {
		if o := f.owner(t, id); o != nil {
			t.Fatalf("-project %d changed #%d in another project", f.p2, id)
		}
	}

	out, code := runBackfill(t, "-apply")
	if code != 0 || !strings.Contains(out, "APPLIED") || !strings.Contains(out, "total: 3 ticket(s) in 1 project(s)") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for id, want := range f.want {
		if o := f.owner(t, id); o == nil || *o != want {
			t.Errorf("#%d owner = %v, want %d", id, o, want)
		}
	}
	for id, want := range f.untouched {
		o := f.owner(t, id)
		if (o == nil) != (want == nil) || (o != nil && *o != *want) {
			t.Errorf("#%d owner = %v, want unchanged %v", id, o, want)
		}
	}

	// One audit entry per project changed, in the project's org chain, naming the tickets.
	rows, err := f.d.db.Query(context.Background(), `SELECT project_id, org_id, actor_email, actor_type, client, chain_key, details->'task_ids'
		FROM audit_log WHERE action = 'task.owner.backfill' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		project, org                  int
		actor, actorType, client, key string
		ids                           []int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.project, &r.org, &r.actor, &r.actorType, &r.client, &r.key, &r.ids); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 2 || got[0].project != f.p2 || !slices.Equal(got[0].ids, f.p2Want) || got[0].actor != "ops@x" ||
		got[1].project != f.p1 || !slices.Equal(got[1].ids, f.p1Want) || got[1].actor != "amp-api backfill-owners" ||
		got[1].org != f.org || got[1].actorType != "system" || got[1].client != "cli" || got[1].key != "org:"+strconv.Itoa(f.org) {
		t.Fatalf("audit rows: %+v", got)
	}
	// Unsigned in tests, so "unproven" at best; what matters is no breaks.
	if rep := f.d.verify(t); problemKinds(rep) != "" {
		t.Fatalf("audit chain after the backfill: %s", problemKinds(rep))
	}

	// Again: nothing left, no new audit rows.
	out, _ = runBackfill(t, "-apply")
	if !strings.Contains(out, "total: 0 ticket(s) in 0 project(s)") || f.auditRows(t) != 2 {
		t.Fatalf("second apply: %s (audit rows %d)", out, f.auditRows(t))
	}
}

func TestBackfillOwners_Usage(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"extra"}, {"-project", "-1"}} {
		var out, errOut bytes.Buffer
		if code := runBackfillOwnersCLI(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: amp-api backfill-owners") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

// If the audit entry can't be written, no ticket changes: one transaction.
func TestBackfillOwners_ApplyIsAllOrNothing(t *testing.T) {
	f := newOwnerFixture(t)
	t.Setenv("DATABASE_URL", f.d.dsn)
	f.d.exec(t, `CREATE FUNCTION refuse_backfill_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action = 'task.owner.backfill' THEN RAISE EXCEPTION 'audit refused'; END IF; RETURN NEW; END $$`)
	f.d.exec(t, `CREATE TRIGGER refuse_backfill_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION refuse_backfill_audit()`)

	if _, code := runBackfill(t, "-apply"); code != 2 {
		t.Fatalf("apply with a failing audit write: exit %d, want 2", code)
	}
	for id := range f.want {
		if o := f.owner(t, id); o != nil {
			t.Errorf("#%d got owner %d although the audit write failed", id, *o)
		}
	}
}
