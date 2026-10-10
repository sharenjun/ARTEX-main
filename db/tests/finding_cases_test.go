//go:build ignore

package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func caseTestDB(t *testing.T) (*DB, *Task, []int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		// An explicit test database must never silently skip a broken migration in CI.
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("open configured test database: %v", err)
		}
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("case integration", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, sev := range []string{"high", "medium", "low", "high"} {
		id, err := d.AddFinding(task.ID, 0, "IDOR", "order access", sev, "GET /orders/{id}", "original PoC "+sev, "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); _ = d.DeleteTask(task.ID) })
	return d, task, ids
}
func TestFindingCaseMergePreservesRecordsAndVersions(t *testing.T) {
	d, task, ids := caseTestDB(t)
	ctx := context.Background()
	id, err := d.MergeFindingCase(ctx, task.ID, ids[:3], "订单越权", "同一接口同一权限检查缺失", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.GetFindingCase(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Count != 3 || c.High != 1 || c.Medium != 1 || c.Low != 1 {
		t.Fatalf("counts %+v", c)
	}
	stats, err := d.DistinctFindingStats(fmt.Sprint(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := d.DistinctFindingStatsByTask()
	if err != nil || batch[fmt.Sprint(task.ID)] == nil || *batch[fmt.Sprint(task.ID)] != *stats {
		t.Fatalf("batch stats %+v %v", batch, err)
	}
	if stats.Total != 2 || stats.Reports != 4 || stats.Unassessed != 1 {
		t.Fatalf("stats %+v", stats)
	}
	again, err := d.MergeFindingCase(ctx, task.ID, ids[:3], "same", "same", "reporter")
	if err != nil || again != id {
		t.Fatalf("idempotency %d %v", again, err)
	}
	same, _ := d.GetFindingCase(id)
	if same.Version != c.Version {
		t.Fatal("idempotent merge invalidated report")
	}
	if err := d.UpdateFindingCaseReport(ctx, id, c.Version, "订单越权", "complete PoC", "high", "verified sensitive data"); err != nil {
		t.Fatal(err)
	}
	for i, fid := range ids[:3] {
		f, err := d.GetFinding(fid)
		if err != nil {
			t.Fatal(err)
		}
		if f.Severity != []string{"high", "medium", "low"}[i] || f.Evidence == "" || f.Status != FindingPending {
			t.Fatalf("original overwritten %+v", f)
		}
	}
	if _, err := d.Exec(`UPDATE findings SET evidence='new proof' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateFindingCaseReport(ctx, id, c.Version, "x", "stale", "low", "x"); err != ErrFindingCaseConflict {
		t.Fatalf("stale accepted %v", err)
	}
	if err := d.RemoveFindingCaseMember(ctx, id, ids[1], "different cause"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MergeFindingCase(ctx, task.ID, ids[:3], "x", "x", "reporter"); err == nil {
		t.Fatal("human veto ignored")
	}
	if err := d.RemoveFindingCaseMember(ctx, id, ids[2], "undo"); err != nil {
		t.Fatal(err)
	}
	c, _ = d.GetFindingCase(id)
	if c.Active {
		t.Fatal("singleton remains group")
	}
	for _, fid := range ids[:3] {
		g, err := d.FindingCaseID(fid)
		if err != nil || g != 0 {
			t.Fatalf("dangling membership %d %v", g, err)
		}
	}
}
func TestFindingCaseScopeSuggestionsAndDeletion(t *testing.T) {
	d, task, ids := caseTestDB(t)
	ctx := context.Background()
	other, err := d.CreateTask("other", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	fid, err := d.AddFinding(other.ID, 0, "IDOR", "other", "high", "same", "poc", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM findings WHERE id=$1`, fid)
	if _, err := d.MergeFindingCase(ctx, task.ID, []int64{ids[0], fid}, "x", "x", "reporter"); err == nil {
		t.Fatal("cross task accepted")
	}
	sid, err := d.SuggestFindingCase(ctx, task.ID, ids[0], ids[1], "order", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ResolveFindingCaseSuggestion(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MergeFindingCase(ctx, task.ID, ids[:2], "x", "x", "reporter"); err == nil {
		t.Fatal("rejection ignored")
	}
	cid, err := d.MergeFindingCase(ctx, task.ID, ids[2:], "x", "proof", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteFinding(ids[3]); err != nil {
		t.Fatal(err)
	}
	c, _ := d.GetFindingCase(cid)
	if c.Active {
		t.Fatal("deletion did not dissolve group")
	}
	f, err := d.GetFinding(ids[2])
	if err != nil || f == nil {
		t.Fatal("surviving finding lost")
	}
}
func TestFindingCaseConcurrentUnion(t *testing.T) {
	d, task, ids := caseTestDB(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, pair := range [][]int64{{ids[0], ids[1]}, {ids[1], ids[2]}, {ids[2], ids[3]}} {
		pair := pair
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.MergeFindingCase(ctx, task.ID, pair, "orders", "same authorization check", "reporter")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	group := int64(0)
	for _, fid := range ids {
		cid, err := d.FindingCaseID(fid)
		if err != nil || cid == 0 {
			t.Fatalf("membership %v", err)
		}
		if group != 0 && cid != group {
			t.Fatal("overlapping parallel groups")
		}
		group = cid
	}
	c, _ := d.GetFindingCase(group)
	if c.Count != 4 {
		t.Fatalf("lost member %+v", c)
	}
	rows, total, err := d.ListFindingCases(FindingFilter{TaskID: fmt.Sprint(task.ID), Severity: "low"}, 1, 10)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Case.Count != 4 || len(rows[0].MatchedIDs) != 1 {
		t.Fatalf("filter/aggregation %d %+v %v", total, rows, err)
	}
}

func TestFindingCaseTaskDeletionAndRepeatedDetach(t *testing.T) {
	d, task, ids := caseTestDB(t)
	ctx := context.Background()
	cid, err := d.MergeFindingCase(ctx, task.ID, ids[:3], "orders", "same missing permission", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	// A source task can disappear without deleting standalone reports or breaking old links.
	if err := d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, id := range ids {
			_, _ = d.DeleteFinding(id)
		}
	}()
	c, err := d.GetFindingCase(cid)
	if err != nil || c == nil || c.TaskID != nil || c.Count != 3 {
		t.Fatalf("retention %+v %v", c, err)
	}
	if err := d.RemoveFindingCaseMember(ctx, cid, ids[0], "undo"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveFindingCaseMember(ctx, cid, ids[0], "retry"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveFindingCaseMember(ctx, cid, ids[1], "undo"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveFindingCaseMember(ctx, cid, ids[2], "released by dissolution"); err != nil {
		t.Fatal(err)
	}
	c, _ = d.GetFindingCase(cid)
	if c.Active || c.Count != 0 {
		t.Fatalf("folder not dissolved %+v", c)
	}
	for _, id := range ids[:3] {
		f, e := d.GetFinding(id)
		if e != nil || f == nil || f.Evidence == "" {
			t.Fatalf("original lost %d %v", id, e)
		}
	}
}

func TestFindingCaseSuggestionResolutionIsAtomic(t *testing.T) {
	d, task, ids := caseTestDB(t)
	ctx := context.Background()
	sid, err := d.SuggestFindingCase(ctx, task.ID, ids[0], ids[1], "orders", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, accept := range []bool{true, false} {
		wg.Add(1)
		go func(a bool) { defer wg.Done(); _, e := d.ResolveFindingCaseSuggestion(ctx, sid, a); results <- e }(accept)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("%d successful conflicting resolutions", success)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM finding_case_suggestions WHERE id=$1`, sid).Scan(&state); err != nil {
		t.Fatal(err)
	}
	cid, _ := d.FindingCaseID(ids[0])
	if (state == "accepted") != (cid > 0) {
		t.Fatalf("inconsistent resolution %s %d", state, cid)
	}
}

func TestFindingCaseCandidatesAreCompactAndRelated(t *testing.T) {
	d, task, ids := caseTestDB(t)
	if _, err := d.Exec(`UPDATE findings SET vulnclass='SQLi',name='search injection' WHERE id=$1`, ids[3]); err != nil {
		t.Fatal(err)
	}
	items, total, err := d.FindingCaseCandidates(ids[0], 1, 1)
	if err != nil || total != 2 || len(items) != 1 {
		t.Fatalf("candidate pagination %d %+v %v", total, items, err)
	}
	if _, ok := items[0]["report"]; ok {
		t.Fatal("report included in candidate summary")
	}
	if _, ok := items[0]["evidence"]; ok {
		t.Fatal("evidence included in candidate summary")
	}
	cid, err := d.MergeFindingCase(context.Background(), task.ID, ids[:2], "orders", "proof", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveFindingCaseMember(context.Background(), cid, ids[1], "different cause"); err != nil {
		t.Fatal(err)
	}
	_, total, err = d.FindingCaseCandidates(ids[0], 1, 20)
	if err != nil || total != 1 {
		t.Fatalf("veto excluded %d %v", total, err)
	}
}

func TestFindingCaseReviewVerifiesSavedReports(t *testing.T) {
	for _, scenario := range []string{"independent", "suggestion", "missing_original", "stale_original", "missing_case", "current_case", "stale_case", "deleted_finding", "deleted_task", "model_failure"} {
		t.Run(scenario, func(t *testing.T) {
			d, task, ids := caseTestDB(t)
			ctx := context.Background()
			if _, err := d.Exec(`UPDATE findings SET report='original report',report_evidence_version=evidence_version WHERE task_id=$1`, task.ID); err != nil {
				t.Fatal(err)
			}
			conv, err := d.CreateConversation("reporter", "review outputs", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID) })
			if _, err := d.Exec(`INSERT INTO finding_case_review_runs(conversation_id,task_id,finding_ids,state) VALUES($1,$2,$3,'running')`, conv.ID, task.ID, fmt.Sprintf("[%d,%d]", ids[0], ids[1])); err != nil {
				t.Fatal(err)
			}
			wantState, wantError := "done", ""
			switch scenario {
			case "suggestion":
				if _, err := d.SuggestFindingCase(ctx, task.ID, ids[0], ids[1], "possible duplicate", "insufficient evidence"); err != nil {
					t.Fatal(err)
				}
			case "missing_original":
				if _, err := d.Exec(`UPDATE findings SET report='' WHERE id=$1`, ids[0]); err != nil {
					t.Fatal(err)
				}
				wantState, wantError = "failed", "独立报告"
			case "stale_original":
				if _, err := d.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, ids[0]); err != nil {
					t.Fatal(err)
				}
				wantState, wantError = "failed", "独立报告"
			case "missing_case", "current_case", "stale_case":
				cid, err := d.MergeFindingCase(ctx, task.ID, ids[:2], "orders", "same verified defect", "reporter")
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "missing_case" {
					c, err := d.GetFindingCase(cid)
					if err != nil {
						t.Fatal(err)
					}
					if err := d.UpdateFindingCaseReport(ctx, cid, c.Version, "orders", "complete unified report", "medium", "verified impact"); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "stale_case" {
					if _, err := d.Exec(`UPDATE findings SET summary=summary || ' changed' WHERE id=$1`, ids[0]); err != nil {
						t.Fatal(err)
					}
				}
				if scenario != "current_case" {
					wantState, wantError = "failed", "统一报告"
				}
			case "deleted_finding":
				if _, err := d.DeleteFinding(ids[0]); err != nil {
					t.Fatal(err)
				}
				wantState, wantError = "failed", "独立报告"
			case "deleted_task":
				if err := d.DeleteTask(task.ID); err != nil {
					t.Fatal(err)
				}
				wantState, wantError = "failed", "来源任务"
			case "model_failure":
				wantState, wantError = "failed", "model unavailable"
			}
			reason := ""
			if scenario == "model_failure" {
				reason = wantError
			}
			if err := d.FinishFindingCaseReview(ctx, conv.ID, scenario != "model_failure", reason); err != nil {
				t.Fatal(err)
			}
			var state, message string
			if err := d.QueryRow(`SELECT state,error FROM finding_case_review_runs WHERE conversation_id=$1`, conv.ID).Scan(&state, &message); err != nil {
				t.Fatal(err)
			}
			if state != wantState || !strings.Contains(message, wantError) {
				t.Fatalf("state=%q error=%q; want %q containing %q", state, message, wantState, wantError)
			}
			// Follow-up turns cannot overwrite a sealed review outcome.
			if err := d.FinishFindingCaseReview(ctx, conv.ID, false, "later turn failed"); err != nil {
				t.Fatal(err)
			}
			var sealedState, sealedError string
			if err := d.QueryRow(`SELECT state,error FROM finding_case_review_runs WHERE conversation_id=$1`, conv.ID).Scan(&sealedState, &sealedError); err != nil {
				t.Fatal(err)
			}
			if sealedState != state || sealedError != message {
				t.Fatal("sealed outcome changed")
			}
		})
	}
}
