//go:build ignore

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func plannerHandoffServer(t *testing.T) (*Server, *Task, func(string, string, string) *httptest.ResponseRecorder) {
	t.Helper()
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("isolated ARTEX_PG_DSN required")
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	off := false
	if err := m.pg.SetPlannerHandoffSettings(t.Context(), &off, &off); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.SetPlannerHandoffSettings(context.Background(), &off, &off) })
	task, err := m.CreateTask("handoff integration", "verify the existing bounded goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { _ = m.pg.DeleteTask(taskID) })
	s := newManagedTestServer(t, m, t.TempDir(), t.TempDir(), t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	return s, task, request
}

func TestPlannerHandoffSettingsAndReadOnlyView(t *testing.T) {
	s, task, request := plannerHandoffServer(t)
	viewPath := "/api/tasks/" + task.ID + "/planner-handoff"
	before := len(s.taskAgents)
	w := request("GET", viewPath, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var initial struct {
		Options  db.PlannerHandoffOptions `json:"options"`
		Snapshot *db.PlannerHandoff       `json:"snapshot"`
		Status   struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Options.Save || initial.Options.Restore || initial.Snapshot != nil || initial.Status.State != "disabled" {
		t.Fatal(w.Body)
	}
	if len(s.taskAgents) != before {
		t.Fatal("read-only view assembled an Agent")
	}
	w = request("PUT", "/api/settings", `{"planner_handoff_save":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	opts, err := s.m.pg.ReadPlannerHandoffOptions(t.Context())
	if err != nil || !opts.Save || opts.Restore {
		t.Fatal(opts, err)
	}
	w = request("PUT", "/api/settings", `{"planner_handoff_restore":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	opts, err = s.m.pg.ReadPlannerHandoffOptions(t.Context())
	if err != nil || !opts.Save || !opts.Restore {
		t.Fatal(opts, err)
	}
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", viewPath, nil))
	if unauth.Code != 401 {
		t.Fatalf("handoff management view must require auth: %d", unauth.Code)
	}
}

func TestPlannerHandoffSettingsBusyLeavesOptionsUnchanged(t *testing.T) {
	s, task, request := plannerHandoffServer(t)
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	core, err := s.m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Rollback()
	var locked int64
	if err = core.QueryRowContext(t.Context(), `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	response := request("PUT", "/api/settings", `{"planner_handoff_save":true,"planner_handoff_restore":true}`)
	if response.Code != 409 {
		t.Fatalf("expected bounded conflict: %d %s", response.Code, response.Body)
	}
	opts, err := s.m.pg.ReadPlannerHandoffOptions(t.Context())
	if err != nil || opts.Save || opts.Restore {
		t.Fatalf("failed settings changed options: %+v %v", opts, err)
	}
	if _, err = core.ExecContext(t.Context(), `SET LOCAL lock_timeout='500ms'; LOCK TABLE tasks IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal("auxiliary settings blocked completion", err)
	}
	if err = core.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPlannerHandoffAssemblyRevocationAndLifecycle(t *testing.T) {
	s, task, request := plannerHandoffServer(t)
	w := request("PUT", "/api/settings", `{"planner_handoff_save":true,"planner_handoff_restore":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	old := s.agentsForTask(task).pl
	ctx := t.Context()
	lease, _, err := s.m.pg.BeginPlannerHandoff(ctx, taskID, task.ExpID, old.HandoffInstanceID(), "first-round")
	if err != nil {
		t.Fatal(err)
	}
	basis, err := s.m.pg.CurrentPlannerHandoffBasis(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	content := db.PlannerHandoffContent{FormatVersion: 1, BackgroundMissing: true, Basis: basis, Todos: []db.PlannerHandoffTodo{{ID: "stable-item", Content: "Keep an unverified bounded check visible", Status: "pending"}}}
	snapshot, err := s.m.pg.CommitPlannerHandoff(ctx, *lease, "first-request", 0, content, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.invalidateTaskAgents()
	fresh := s.agentsForTask(task).pl
	if fresh.HandoffInstanceID() == old.HandoffInstanceID() {
		t.Fatal("reassembly reused publisher identity")
	}
	if _, _, err = s.m.pg.BeginPlannerHandoff(ctx, taskID, task.ExpID, old.HandoffInstanceID(), "late-round"); !errors.Is(err, db.ErrPlannerHandoffStale) {
		t.Fatalf("old instance reacquired: %v", err)
	}
	current, err := s.m.pg.GetPlannerHandoff(ctx, taskID)
	if err != nil || current.Version != snapshot.Version || current.Content.Todos[0].ID != "stable-item" {
		t.Fatal(current, err)
	}
	lease, _, err = s.m.pg.BeginPlannerHandoff(ctx, taskID, task.ExpID, fresh.HandoffInstanceID(), "second-round")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.ApplyTaskPause(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.m.ApplyTaskAdmission(task.ID, task.lifecycleSnapshot().Status, task.lifecycleSnapshot().Status, false, "", false); err != nil {
		t.Fatal(err)
	}
	content.Todos[0].Content = "old result after pause/resume"
	if _, err = s.m.pg.CommitPlannerHandoff(ctx, *lease, "late-request", snapshot.Version, content, time.Now()); !errors.Is(err, db.ErrPlannerHandoffStale) {
		t.Fatalf("pause/resume failed to fence old lease: %v", err)
	}
	if _, _, err = s.m.pg.BeginPlannerHandoff(ctx, taskID, task.ExpID, fresh.HandoffInstanceID(), "third-round"); err != nil {
		t.Fatal("active instance did not continue after resume", err)
	}
	w = request("GET", "/api/tasks/"+task.ID+"/planner-handoff?history=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "stable-item") {
		t.Fatal(w.Code, w.Body)
	}
	if _, err = s.m.pg.Exec(`UPDATE planner_handoffs SET content_hash='damaged' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/tasks/"+task.ID+"/planner-handoff", "")
	if w.Code != 503 {
		t.Fatal("damaged content must not appear as no data", w.Code, w.Body)
	}
	if task.Store.ID() != task.ExpID {
		t.Fatal("handoff failure modified task graph")
	}
}
