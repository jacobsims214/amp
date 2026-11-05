package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/receipts"
	"github.com/simstech/amp-api/internal/service"
)

// Verification receipts (#126). amp_open_receipt opens a completion's draft
// receipt; amp_add_evidence mints one-time upload URLs for its evidence; amp_complete_task takes the receipt_id and the checks the worker
// ran. Accepting or rejecting a receipt is a person's, in the web UI only:
// no tool writes a person's verdict.

// receiptProps are amp_complete_task's receipt arguments.
func receiptProps() props {
	return props{
		"receipt_id": num("Optional. The receipt_id amp_open_receipt returned. Without it the completion adopts the ticket's newest " +
			"draft, or makes one, when the project records receipts."),
		"checks": checksArr("Optional. One check per acceptance criterion you verified: {criterion_ref (the criterion's 1-based number, " +
			"e.g. \"2\"), kind: command|test|ci|visual|manual, verdict: pass|fail|cant_tell, command?, exit_code?, duration_ms?, note?, " +
			"evidence_key?}. Legacy {ok: true|false} means pass|fail. At most 100; notes at most 2 KB. Report what happened: a check " +
			"you couldn't run is cant_tell, never pass."),
		"files_changed": filesArr("Optional. The files you changed: [{path, added, removed}]."),
		"tree_sha":      str("Optional. The git tree SHA of the revision you checked (git rev-parse HEAD^{tree})."),
	}
}

func checksArr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "array", "description": desc, "items": map[string]interface{}{"type": "object"}}
}

func filesArr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "array", "description": desc, "items": map[string]interface{}{"type": "object"}}
}

func (s *Server) registerReceipts(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_open_receipt",
		"Open the verification receipt for your ticket before you complete it, as its worker (your current dispatch_id). "+
			"Returns {receipt_id, mode}. When mode is \"off\" the project doesn't record receipts: receipt_id is 0 and you complete "+
			"exactly as before. Otherwise pass receipt_id and one check per acceptance criterion to amp_complete_task. A second call "+
			"for the same dispatch returns the same receipt.",
		props{
			"task_id":       num("REQUIRED. Task ID."),
			"dispatch_id":   str("REQUIRED. The dispatch_id you were dispatched with."),
			"summary":       str("Optional. One or two sentences: what you did."),
			"files_changed": filesArr("Optional. The files you changed: [{path, added, removed}]."),
		}), wrap(ctx, s.guard(authz.Member, s.openReceipt)))
	mcp.AddTool(tool("amp_add_evidence",
		"Declare the evidence files you recorded for your open receipt (screenshots, a video, logs) and get a one-time upload "+
			"URL for each. Upload each file's bytes with an HTTP PUT to its url (no auth header: the URL is the credential; single "+
			"use, valid 15 minutes): `curl -fsS -T screenshot-1.png \"$url\"` (prefix a bare /api/... path with $AMP_URL). "+
			"Then cite the key as a visual check's evidence_key in amp_complete_task. A visual pass without its own stored "+
			"evidence, on the revision you complete, counts as cant_tell. Names: "+receipts.AllowedNames+". Evidence isn't "+
			"an artifact: it is kept with the receipt, and deleted after 90 days unless the ticket merges or a person accepts it. "+
			"Re-adding a file gives a new URL (a retry), or no url when it is already stored with the same sha256.",
		props{
			"receipt_id":  num("REQUIRED. The receipt_id amp_open_receipt returned."),
			"dispatch_id": str("REQUIRED. Your dispatch_id: the one that opened the receipt."),
			"tree_sha":    str("REQUIRED. The git tree of the revision you recorded on: git rev-parse HEAD^{tree}."),
			"commit":      str("Optional. The commit SHA you recorded on: git rev-parse HEAD."),
			"files": map[string]interface{}{"type": "array", "description": "REQUIRED. 1 to 20 files: [{name, bytes, sha256, " +
				"duration_ms?, frames?}]. bytes is the exact size, sha256 the lowercase hex digest (sha256sum). frames: a video's " +
				"frame count (0 means it recorded nothing).", "items": map[string]interface{}{"type": "object"}},
		}), wrap(ctx, s.open(s.addEvidence))) // AddEvidence checks TaskWork on the receipt's ticket
}

func (s *Server) addEvidence(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	receiptID, err := reqInt(args, "receipt_id")
	if err != nil {
		return nil, err
	}
	raw, ok := args["files"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("files is required: [{name, bytes, sha256, duration_ms?, frames?}]")
	}
	files := make([]service.EvidenceFileIn, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("files[%d] must be {name, bytes, sha256, duration_ms?, frames?}", i)
		}
		f := service.EvidenceFileIn{}
		f.Name, _ = m["name"].(string)
		f.Name = strings.TrimSpace(f.Name)
		f.SHA256, _ = m["sha256"].(string)
		f.SHA256 = strings.ToLower(strings.TrimSpace(f.SHA256))
		n, ok := m["bytes"].(float64)
		if !ok || n != float64(int64(n)) {
			return nil, fmt.Errorf("files[%d].bytes must be an integer", i)
		}
		f.Bytes = int64(n)
		for key, dst := range map[string]**int{"duration_ms": &f.DurationMS, "frames": &f.Frames} {
			if v, ok := m[key]; ok && v != nil {
				x, ok := v.(float64)
				if !ok || x != float64(int(x)) {
					return nil, fmt.Errorf("files[%d].%s must be an integer", i, key)
				}
				iv := int(x)
				*dst = &iv
			}
		}
		files = append(files, f)
	}
	ups, err := s.svc.AddEvidence(rctx, caller(rctx).p, int64(receiptID), optStr(args, "dispatch_id", ""), optStr(args, "commit", ""),
		optStr(args, "tree_sha", ""), files)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"uploads": ups,
		"next": "PUT each file's bytes to its url (curl -fsS -T <file> \"<url>\"), then cite each key as a visual check's evidence_key " +
			"in amp_complete_task. An entry with no url is already stored."})
}

func (s *Server) openReceipt(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	files, err := optFilesChanged(args)
	if err != nil {
		return nil, err
	}
	rc, mode, err := s.svc.OpenReceipt(rctx, caller(rctx).p, taskID, optStr(args, "dispatch_id", ""), optStr(args, "summary", ""), files)
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return jsonResult(map[string]interface{}{"receipt_id": 0, "mode": domain.ReceiptsOff,
			"next": "This project doesn't record receipts: complete exactly as before (amp_complete_task with your dispatch_id)."})
	}
	next := "Do the work, then amp_complete_task with this receipt_id and checks[]: one per acceptance criterion, with the verdict " +
		"you saw (pass, fail, or cant_tell when you couldn't check it)."
	if mode == domain.ReceiptsRequire {
		next += " This ticket requires a receipt: below self-checked (no passing check, or any failing one) the completion is held for a person."
	}
	return jsonResult(map[string]interface{}{"receipt_id": rc.ID, "mode": mode, "next": next})
}

// optFilesChanged reads files_changed (nil when absent).
func optFilesChanged(args map[string]interface{}) ([]domain.FileChange, error) {
	v, ok := args["files_changed"]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("files_changed must be an array of {path, added, removed}")
	}
	return domain.ParseFilesChanged(raw)
}

// receiptFromArgs reads amp_complete_task's receipt arguments.
func receiptFromArgs(args map[string]interface{}, dispatchID string) (*service.ReceiptIn, error) {
	in := &service.ReceiptIn{DispatchID: dispatchID, TreeSHA: strings.TrimSpace(optStr(args, "tree_sha", ""))}
	if v, ok := args["receipt_id"].(float64); ok && v > 0 {
		in.ReceiptID = int64(v)
	}
	if v, ok := args["checks"]; ok && v != nil {
		raw, ok := v.([]interface{})
		if !ok {
			return nil, fmt.Errorf("checks must be an array of {criterion_ref, kind, verdict, ...}")
		}
		checks, err := domain.ParseChecks(raw)
		if err != nil {
			return nil, err
		}
		in.Checks = checks
	}
	files, err := optFilesChanged(args)
	if err != nil {
		return nil, err
	}
	in.FilesChanged = files
	return in, nil
}
