//go:build ignore

package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
)

func testCaseServer(t *testing.T) (*Server, int64, []int64) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		// An explicit test database must never silently skip a broken migration in CI.
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("open configured test database: %v", err)
		}
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { pg.Close() })
	task, err := pg.CreateTask("case server", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, sev := range []string{"high", "medium", "low"} {
		id, err := pg.AddFinding(task.ID, 0, "IDOR", "orders", sev, "same endpoint", "proof "+sev, "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); _ = pg.DeleteTask(task.ID) })
	return &Server{m: &Manager{pg: pg}}, task.ID, ids
}
func TestFindingCaseAPIPaginationAndTools(t *testing.T) {
	s, task, ids := testCaseServer(t)
	ctx := context.Background()
	a := findingCaseRequest{TaskID: json.RawMessage(fmt.Sprint(task)), Title: "订单越权", Reason: "same check"}
	for _, id := range ids {
		a.IDs = append(a.IDs, json.RawMessage(fmt.Sprint(id)))
	}
	result, err := s.performFindingCaseTool(ctx, "merge_finding_records", a)
	if err != nil {
		t.Fatal(err)
	}
	cid := result.(map[string]string)["case_id"]
	r := httptest.NewRequest("GET", fmt.Sprintf("/api/exploration/finding-cases?task_id=%d&severity=low&page=1&limit=1", task), nil)
	w := httptest.NewRecorder()
	s.listFindingCases(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var page struct {
		Total int `json:"total"`
		Items []struct {
			Case    db.FindingCase `json:"case"`
			Matched []int64        `json:"matched_ids"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Items[0].Case.Count != 3 || page.Items[0].Case.Report != "" || len(page.Items[0].Matched) != 1 {
		t.Fatalf("page %+v", page)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", cid)
	w = httptest.NewRecorder()
	s.getFindingCaseMembers(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "proof high") {
		t.Fatal(w.Code, w.Body)
	}
	a.CaseID = json.RawMessage(cid)
	result, err = s.performFindingCaseTool(ctx, "get_finding_case", a)
	if err != nil {
		t.Fatal(err)
	}
	c := result.(map[string]any)["case"].(*db.FindingCase)
	a.Version = c.Version
	a.Report = "complete reproducible report"
	a.Severity = "high"
	a.SeverityReason = "verified data disclosure"
	if _, err := s.performFindingCaseTool(ctx, "update_finding_case_report", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE findings SET evidence='new data' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.performFindingCaseTool(ctx, "update_finding_case_report", a); err != db.ErrFindingCaseConflict {
		t.Fatalf("stale report accepted %v", err)
	}
}
func TestFindingCaseConsolidatedExportPreservesScope(t *testing.T) {
	s, task, ids := testCaseServer(t)
	ctx := context.Background()
	cid, err := s.m.pg.MergeFindingCase(ctx, task, ids, "订单越权", "same", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.m.pg.GetFindingCase(cid)
	if err := s.m.pg.UpdateFindingCaseReport(ctx, cid, c.Version, "订单越权", "complete report", "high", "verified"); err != nil {
		t.Fatal(err)
	}
	fs, err := s.m.pg.ListFindingsForExport(db.FindingFilter{}, ids[:1])
	if err != nil {
		t.Fatal(err)
	}
	expanded, plan, err := s.prepareFindingCaseExport(fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 3 {
		t.Fatal("selected one child did not include complete group")
	}
	out, err := s.consolidateFindingCaseExport(expanded, plan, false)
	if err != nil || len(out) != 1 || out[0].Report != "complete report" || !strings.Contains(out[0].Summary, fmt.Sprint(ids[2])) {
		t.Fatalf("unified %+v %v", out, err)
	}
	out, err = s.consolidateFindingCaseExport(expanded, plan, true)
	if err != nil || len(out) != 4 {
		t.Fatalf("originals lost %d %v", len(out), err)
	}
	if _, err := s.m.pg.Exec(`UPDATE findings SET severity='critical' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	out, err = s.consolidateFindingCaseExport(expanded, plan, false)
	if err != nil || !strings.Contains(out[0].Report, "已过期") || !strings.Contains(out[0].Evidence, "proof low") {
		t.Fatalf("pending export omitted evidence %+v %v", out, err)
	}
}
func TestFindingCaseManualSelectionScope(t *testing.T) {
	s, task, ids := testCaseServer(t)
	conv, err := s.m.pg.CreateConversation("reporter", "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID)
	raw, _ := json.Marshal(ids[:2])
	if _, err := s.m.pg.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids) VALUES($1,$2,$3)`, conv.ID, task, string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(context.Background(), conv.ID)
	a := findingCaseRequest{IDs: []json.RawMessage{json.RawMessage(fmt.Sprint(ids[0])), json.RawMessage(fmt.Sprint(ids[2]))}}
	if err := s.checkCaseReviewScope(ctx, "merge_finding_records", a); err == nil {
		t.Fatal("unselected mutation allowed")
	}
	a.IDs[1] = json.RawMessage(fmt.Sprint(ids[1]))
	if err := s.checkCaseReviewScope(ctx, "merge_finding_records", a); err != nil {
		t.Fatal(err)
	}
}

func TestFindingCaseReviewStatusSurvivesTaskDeletion(t *testing.T) {
	s, task, ids := testCaseServer(t)
	conv, err := s.m.pg.CreateConversation("reporter", "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID)
	if _, err := s.m.pg.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids) VALUES($1,$2,$3)`, conv.ID, task, fmt.Sprintf("[%d]", ids[0])); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.DeleteTask(task); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, id := range ids {
			_, _ = s.m.pg.DeleteFinding(id)
		}
	}()
	w := httptest.NewRecorder()
	s.caseReviewRuns(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"task_id":"0"`) {
		t.Fatal(w.Code, w.Body)
	}
}

func TestFindingCaseSelectedReviewCannotRewriteOtherOriginal(t *testing.T) {
	s, task, ids := testCaseServer(t)
	conv, err := s.m.pg.CreateConversation("reporter", "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID)
	raw, _ := json.Marshal(ids[:1])
	if _, err := s.m.pg.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids) VALUES($1,$2,$3)`, conv.ID, task, string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(context.Background(), conv.ID)
	if err := s.checkCaseReviewScope(ctx, "update_finding_report", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(ids[1]))}); err == nil {
		t.Fatal("unselected original allowed")
	}
	if err := s.checkCaseReviewScope(ctx, "update_finding_report", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(ids[0]))}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingCaseGroupedTrafficExportAndInheritedAccess(t *testing.T) {
	s, recorded, req := trafficEvidenceServer(t)
	ctx := t.Context()
	f, err := s.m.pg.GetFinding(recorded.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.m.pg.GetTask(*f.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("verified original response")
	seedServerEvidenceFlow(t, s, "case-proof", body)
	if w := req("POST", fmt.Sprintf("/api/exploration/findings/%d/traffic", f.ID), `{"traffic_refs":[{"traffic_id":"case-proof","role":"proof"}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	other, err := s.evidenceStore().Record(ctx, db.RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Summary: "same permission boundary", Severity: "low"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.ID, other.FindingID} {
		if _, err := s.m.pg.Exec(`UPDATE findings SET report=$2 WHERE id=$1`, id, fmt.Sprintf("original script #%d", id)); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := s.m.pg.MergeFindingCase(ctx, task.ID, []int64{f.ID, other.FindingID}, "orders", "same permission check", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.m.pg.GetFindingCase(cid)
	if err := s.m.pg.UpdateFindingCaseReport(ctx, cid, c.Version, "orders", "unified complete script", "high", "verified impact"); err != nil {
		t.Fatal(err)
	}
	// A binding note is evidence too: changing it makes the aggregate report stale.
	list, err := s.m.pg.GetFindingTraffic(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := req("PATCH", fmt.Sprintf("/api/exploration/findings/%d/traffic/%d", f.ID, list.Bindings[0].ID), fmt.Sprintf(`{"version":%d,"note":"additional verified condition"}`, list.Version))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	c, _ = s.m.pg.GetFindingCase(cid)
	if c.Version == c.ReportVersion {
		t.Fatal("binding evidence did not invalidate unified report")
	}
	url := fmt.Sprintf("/api/exploration/findings/export?mode=consolidated&include_originals=true&scope=selected&ids=%d&format=md-zip", f.ID)
	w = req("GET", url, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	mdCount, caseCount := 0, 0
	seen := map[string]bool{}
	recovered := false
	for _, file := range z.File {
		if seen[file.Name] {
			t.Fatalf("duplicate ZIP entry %s", file.Name)
		}
		seen[file.Name] = true
		reader, e := file.Open()
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(reader)
		reader.Close()
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasSuffix(file.Name, ".md") {
			mdCount++
			if strings.HasPrefix(file.Name, "case_") {
				caseCount++
				if !strings.Contains(string(data), fmt.Sprintf("original script #%d", other.FindingID)) {
					t.Fatal("stale export lost an original report")
				}
			}
		}
		if strings.HasSuffix(file.Name, "response.bin") && bytes.Equal(data, body) {
			recovered = true
		}
	}
	if mdCount != 3 || caseCount != 1 || !recovered {
		t.Fatalf("ZIP reports=%d folders=%d evidence=%v", mdCount, caseCount, recovered)
	}
	w = req("GET", fmt.Sprintf("/api/exploration/findings/export?mode=consolidated&scope=selected&ids=%d&format=json", f.ID), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id": "case:`) || !strings.Contains(w.Body.String(), `"member_ids"`) {
		t.Fatal(w.Code, w.Body)
	}
	child, err := s.m.CreateTaskWithOptions("inherited folders", "fixture", db.TaskCreateOptions{SourceTaskIDs: []int64{task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := strconv.ParseInt(child.ID, 10, 64)
	defer s.m.pg.DeleteTask(childID)
	base := fmt.Sprintf("/api/exploration/finding-cases/%d", cid)
	suffix := "?context_task=" + child.ID
	if w = req("GET", base+suffix, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w = req("GET", base+"/members"+suffix, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"inherited":true`) {
		t.Fatal(w.Code, w.Body)
	}
	if w = req("DELETE", fmt.Sprintf("%s/members/%d%s", base, f.ID, suffix), `{}`); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	if w = req("PUT", base+"/report"+suffix, `{}`); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	if w = req("POST", "/api/exploration/finding-case-review"+suffix, fmt.Sprintf(`{"case_id":%d}`, cid)); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestFindingCaseCorrectionSurvivesUnavailableReporter(t *testing.T) {
	s, task, ids := testCaseServer(t)
	id, err := s.m.pg.SuggestFindingCase(context.Background(), task, ids[0], ids[1], "orders", "same defect")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"accept":true}`))
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	s.resolveCaseSuggestion(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var result struct {
		CaseID string `json:"case_id"`
		Queued bool   `json:"report_queued"`
		Error  string `json:"report_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CaseID == "0" || result.CaseID == "" || result.Queued || result.Error == "" {
		t.Fatalf("unexpected correction result: %+v", result)
	}
	cid, err := s.m.pg.FindingCaseID(ids[0])
	if err != nil || strconv.FormatInt(cid, 10) != result.CaseID {
		t.Fatal("committed correction was lost", cid, err)
	}
}

// Corrections reuse the existing queue; report generation does not need a second approval.
func TestFindingCaseCorrectionQueuesReport(t *testing.T) {
	for _, accept := range []bool{true, false} {
		t.Run(fmt.Sprintf("accept=%t", accept), func(t *testing.T) {
			s, task, ids := testCaseServer(t)
			reporter, err := s.m.pg.GetAgentByKey("reporter")
			if err != nil {
				t.Fatal(err)
			}
			if reporter == nil {
				reporter, err = s.m.pg.CreateAgent("reporter", "test reporter", "report queue fixture")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM agents WHERE key='reporter'`) })
			}
			if _, err := s.m.pg.Exec(`UPDATE agents SET enabled=true,trigger_run_mode='serial' WHERE key='reporter'`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = s.m.pg.Exec(`UPDATE agents SET enabled=$1,trigger_run_mode=$2 WHERE key='reporter'`, reporter.Enabled, reporter.TriggerRunMode)
				_, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM finding_case_review_runs WHERE task_id=$1)`, task)
			})
			s.ctx = t.Context()
			s.chatAgent = &agent.ChatAgent{}
			s.m.tasks = map[string]*Task{i64s(task): {ID: i64s(task), Description: "case server", Goal: "goal"}}
			s.triggerQ = map[string][]triggeredRun{}
			s.triggerCfg = map[string]triggerBehavior{}
			// Keep the serial lane occupied so the test checks scheduling without a model call.
			s.triggerActive = map[string]int{"reporter": 1}
			id, err := s.m.pg.SuggestFindingCase(t.Context(), task, ids[0], ids[1], "orders", "same defect")
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"accept":%t}`, accept)))
			r.SetPathValue("id", i64s(id))
			w := httptest.NewRecorder()
			s.resolveCaseSuggestion(w, r)
			var result struct {
				CaseID string `json:"case_id"`
				Queued bool   `json:"report_queued"`
				Error  string `json:"report_error"`
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Queued != accept || result.Error != "" {
				t.Fatalf("unexpected scheduling result: %+v", result)
			}
			queue := s.triggerQ["reporter"]
			if !accept {
				if len(queue) != 0 || result.CaseID != "0" {
					t.Fatal("rejected suggestion created report work", result, queue)
				}
				return
			}
			if len(queue) != 1 || queue[0].taskID != task || queue[0].conversationID == 0 || queue[0].mergeable {
				t.Fatalf("unexpected queued report: %+v", queue)
			}
			if !strings.Contains(queue[0].message, "不得等待人工确认") {
				t.Fatal("report prompt introduced an approval gate")
			}
			var state string
			var raw []byte
			if err := s.m.pg.QueryRow(`SELECT state,finding_ids FROM finding_case_review_runs WHERE conversation_id=$1`, queue[0].conversationID).Scan(&state, &raw); err != nil {
				t.Fatal(err)
			}
			var selected []int64
			if err := json.Unmarshal(raw, &selected); err != nil {
				t.Fatal(err)
			}
			if state != "queued" || len(selected) != 2 {
				t.Fatalf("state=%q selected=%v", state, selected)
			}
			seen := map[int64]bool{}
			for _, fid := range selected {
				seen[fid] = true
			}
			if !seen[ids[0]] || !seen[ids[1]] || seen[ids[2]] {
				t.Fatal("correction report expanded outside case members", selected)
			}
		})
	}
}

func TestFindingCaseReviewQueueIsolation(t *testing.T) {
	configs := []triggerBehavior{
		{runMode: "serial", mergeMode: "all"},
		{runMode: "serial", mergeMode: "by_task"},
		{runMode: "serial", mergeMode: "none"},
		{runMode: "parallel", mergeMode: "all"},
	}
	for _, cfg := range configs {
		for _, manualFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%s/manualFirst=%t", cfg.runMode, cfg.mergeMode, manualFirst), func(t *testing.T) {
				manual := triggeredRun{agentKey: "reporter", conversationID: 42, taskID: 72, message: "selected-only report"}
				queue := sameTaskFires(2)
				if manualFirst {
					queue = append([]triggeredRun{manual}, queue...)
				} else {
					queue = []triggeredRun{queue[0], manual, queue[1]}
				}
				s := &Server{triggerQ: map[string][]triggeredRun{"reporter": queue}}
				manualCount := 0
				for len(s.triggerQ["reporter"]) > 0 {
					item := s.nextTriggerRun("reporter", cfg)
					if item.conversationID == manual.conversationID {
						manualCount++
						if item.message != manual.message || item.taskID != manual.taskID {
							t.Fatal("selected review was coalesced with automatic triggers", item)
						}
					} else if strings.Contains(item.message, manual.message) {
						t.Fatal("selected scope leaked into an automatic run", item)
					}
				}
				if manualCount != 1 {
					t.Fatalf("selected review executed %d times", manualCount)
				}
			})
		}
	}
}

// A model's end_turn is not proof that the requested report artifacts were saved.
func TestFindingCaseConversationVerifiesArtifacts(t *testing.T) {
	for _, mode := range []string{"current", "missing", "model_failure", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			s, task, ids := testCaseServer(t)
			s.ctx = t.Context()
			s.m.dir = t.TempDir()
			s.chatBusy = map[string]bool{}
			s.chatCancel = map[string]context.CancelCauseFunc{}
			called := false
			setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				called = true
				if mode == "model_failure" {
					return llm.Message{}, "", llm.Usage{}, errors.New("test model unavailable")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("finished")}}, "end_turn", llm.Usage{}, nil
			}})
			if mode == "current" {
				if _, err := s.m.pg.Exec(`UPDATE findings SET report='saved report',report_evidence_version=evidence_version WHERE id=$1`, ids[0]); err != nil {
					t.Fatal(err)
				}
			}
			c, err := s.m.pg.CreateConversation("reporter", "review artifact finalizer", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, c.ID) })
			if _, err := s.m.pg.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids,state) VALUES($1,$2,$3,'running')`, c.ID, task, fmt.Sprintf("[%d]", ids[0])); err != nil {
				t.Fatal(err)
			}
			if mode == "stopped" {
				ctx, cancel := context.WithCancel(s.ctx)
				cancel()
				s.ctx = ctx
			}
			s.runConversationSync(c, "generate reports", s.convBusyKey(c.ID))
			var state, reason string
			if err := s.m.pg.QueryRow(`SELECT state,error FROM finding_case_review_runs WHERE conversation_id=$1`, c.ID).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if mode == "current" {
				want = "done"
			}
			if state != want || (state == "failed" && reason == "") {
				t.Fatalf("state=%q reason=%q, want %q", state, reason, want)
			}
			if called != (mode != "stopped") {
				t.Fatalf("model called=%t for mode %q", called, mode)
			}
		})
	}
}
