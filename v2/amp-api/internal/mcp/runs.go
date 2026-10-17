package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/simstech/amp-api/internal/domain"
)

// runProps are the optional fields any tool that ends or reports a run
// accepts: what the run used and where its work went. Report what your
// harness can tell you; leave the rest out.
func runProps() props {
	return props{
		"model":              str("Optional. The LLM model that did the work, e.g. 'openrouter/moonshotai/kimi-k3' or 'claude-opus-5-5'."),
		"input_tokens":       num("Optional. Input (prompt) tokens this run used, if your harness reports them (opencode: amp_session_usage)."),
		"output_tokens":      num("Optional. Output tokens, reasoning included."),
		"cache_read_tokens":  num("Optional. Prompt-cache read tokens."),
		"cache_write_tokens": num("Optional. Prompt-cache write tokens."),
		"cost_usd":           num("Optional. Cost in USD, if your harness reports it."),
		"branch":             str("Optional. The branch the work is on."),
		"commit":             str("Optional. The commit SHA (full, or at least 7 characters)."),
		"pr_url":             str("Optional. The pull request URL."),
		"pushed":             boolean("Optional. Whether the commit is pushed where teammates can get it."),
		"merged":             boolean("Optional. Whether the work is merged into the main line. Whoever merges (a person, or the agent told to) records merged=true with amp_report_run."),
		"run_note":           str("Optional. One line about the run, e.g. 'closeout only: implementation was committed by the previous run'."),
	}
}

func withRunProps(p props) props {
	for k, v := range runProps() {
		p[k] = v
	}
	return p
}

func optInt64Ptr(args map[string]interface{}, key string) *int64 {
	if v, ok := args[key]; ok && v != nil {
		if n, ok := v.(float64); ok && n >= 0 {
			i := int64(n)
			return &i
		}
	}
	return nil
}

func optFloatPtr(args map[string]interface{}, key string) *float64 {
	if v, ok := args[key]; ok && v != nil {
		if n, ok := v.(float64); ok && n >= 0 {
			return &n
		}
	}
	return nil
}

func optBoolPtr(args map[string]interface{}, key string) *bool {
	if v, ok := args[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return &b
		}
	}
	return nil
}

func runFromArgs(args map[string]interface{}, outcome string) domain.Run {
	return domain.Run{
		Outcome:          outcome,
		AgentID:          strings.TrimSpace(optStr(args, "agent_id", "")),
		Model:            strings.TrimSpace(optStr(args, "model", "")),
		InputTokens:      optInt64Ptr(args, "input_tokens"),
		OutputTokens:     optInt64Ptr(args, "output_tokens"),
		CacheReadTokens:  optInt64Ptr(args, "cache_read_tokens"),
		CacheWriteTokens: optInt64Ptr(args, "cache_write_tokens"),
		CostUSD:          optFloatPtr(args, "cost_usd"),
		Branch:           strings.TrimSpace(optStr(args, "branch", "")),
		Commit:           strings.TrimSpace(optStr(args, "commit", "")),
		PRURL:            strings.TrimSpace(optStr(args, "pr_url", "")),
		Pushed:           optBoolPtr(args, "pushed"),
		Merged:           optBoolPtr(args, "merged"),
		Note:             strings.TrimSpace(optStr(args, "run_note", "")),
	}
}

// recordRunFromArgs records the run described in args, if it describes
// anything; it is how complete and block carry usage and git details.
func (s *Server) recordRunFromArgs(rctx context.Context, taskID int, args map[string]interface{}, outcome string) (*domain.Run, error) {
	run := runFromArgs(args, outcome)
	if !run.HasDetail() {
		return nil, nil
	}
	r, err := s.svc.RecordRun(rctx, caller(rctx).p, taskID, run)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Server) reportRun(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	run, err := s.svc.RecordRun(rctx, caller(rctx).p, taskID, runFromArgs(args, optStr(args, "outcome", "")))
	if err != nil {
		return nil, err
	}
	return jsonResult(run)
}

func (s *Server) taskRuns(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	runs, err := s.svc.TaskRuns(rctx, caller(rctx).p, taskID)
	if err != nil {
		return nil, err
	}
	var u domain.Usage
	for _, r := range runs {
		u.Add(r)
	}
	return jsonResult(map[string]interface{}{"runs": runs, "usage": u})
}

func (s *Server) trace(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	hits, err := s.svc.Trace(rctx, caller(rctx).p, optStr(args, "commit", ""), optStr(args, "branch", ""), optStr(args, "pr_url", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"tickets": hits,
		"count":   len(hits),
		"next":    "amp_get_ticket_history(task_id) gives each ticket's full story: plan, comments, state changes, runs.",
	})
}

func (s *Server) listDeleted(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	trash, err := s.svc.DeletedTasks(rctx, caller(rctx).p, projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"deleted": trash, "count": len(trash)})
}

func (s *Server) restoreTasks(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	ids := optIntSlice(args, "task_ids")
	if len(ids) == 0 {
		return nil, fmt.Errorf("task_ids is required")
	}
	res, err := s.svc.RestoreTasks(rctx, caller(rctx).p, projectID, ids)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"results": res})
}

// compactTasks is amp_list_tasks' small overview: one line per ticket under
// its epic, and the buckets as IDs.
func (s *Server) compactTasks(rctx context.Context, projectID int, tasks []domain.Task, ownerMode string, quiet []map[string]interface{}) (*mcpgo.CallToolResult, error) {
	epics, err := s.repo.ListEpics(rctx, projectID)
	if err != nil {
		return nil, err
	}
	name := map[int]string{}
	for _, e := range epics {
		name[e.ID] = e.Name
	}
	state := func(t domain.Task) string {
		switch {
		case t.State == domain.TaskStateBlocked && strings.HasPrefix(t.BlockReason, "scheduled:"):
			return "scheduled"
		case t.State == domain.TaskStateBlocked && t.BlockReason != "":
			return "question"
		case t.State == domain.TaskStateBlocked:
			return "waiting"
		case t.State == domain.TaskStateBacklog:
			return "ready"
		}
		return string(t.State)
	}
	type line struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		State   string `json:"state"`
		Owner   *int   `json:"owner_user_id,omitempty"`
		WaitsOn []int  `json:"waits_on,omitempty"`
	}
	byEpic := map[string][]line{}
	buckets := map[string][]int{"ready": {}, "in_progress": {}, "question": {}, "waiting": {}, "scheduled": {}, "completed": {}}
	for _, t := range tasks {
		st := state(t)
		buckets[st] = append(buckets[st], t.ID)
		l := line{ID: t.ID, Name: t.Name, State: st, Owner: t.OwnerUserID}
		if st == "waiting" {
			l.WaitsOn = t.BlockedByIDs
		}
		byEpic[name[t.EpicID]] = append(byEpic[name[t.EpicID]], l)
	}
	return jsonResult(map[string]interface{}{
		"ready_to_dispatch": buckets["ready"],
		"in_progress":       buckets["in_progress"],
		"questions":         buckets["question"],
		"waiting":           len(buckets["waiting"]),
		"scheduled":         len(buckets["scheduled"]),
		"completed":         len(buckets["completed"]),
		"quiet":             quiet,
		"by_epic":           byEpic,
		"count":             len(tasks),
		"owner_filter":      ownerMode,
	})
}

func (s *Server) listUnmerged(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	runs, err := s.svc.UnmergedWork(rctx, caller(rctx).p, projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"waiting_to_merge": runs, "count": len(runs),
		"note": "Completed tickets whose pushed work no run has reported merged. After merging, record it: amp_report_run(task_id, outcome='completed', merged=true, commit=<merge sha>)."})
}

func (s *Server) projectDayReports(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	day, rows, err := s.svc.ProjectDayReports(rctx, caller(rctx).p, projectID, optStr(args, "date", ""), optStr(args, "tz", ""))
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, r := range rows {
		if r.Report == nil {
			missing = append(missing, r.Email)
		}
	}
	return jsonResult(map[string]interface{}{"day": day, "people": rows, "no_report_yet": missing})
}

func (s *Server) handover(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	from, to := optStr(args, "from", ""), optStr(args, "to", "")
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to are required (user id or email)")
	}
	inProg, _ := args["include_in_progress"].(bool)
	res, err := s.svc.Handover(rctx, caller(rctx).p, projectID, from, to, optIntSlice(args, "task_ids"), inProg, optStr(args, "reason", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"results": res})
}
