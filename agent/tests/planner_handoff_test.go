//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type handoffTestStore struct {
	options                           db.PlannerHandoffOptions
	basis                             db.PlannerHandoffBasis
	current                           *db.PlannerHandoff
	beginCalls, commitCalls, getCalls int
	commitErr, errorOptions           error
	lostAcknowledgement               bool
	invalidReferences                 bool
}

func (s *handoffTestStore) ReadPlannerHandoffOptions(context.Context) (db.PlannerHandoffOptions, error) {
	return s.options, s.errorOptions
}
func (s *handoffTestStore) BeginPlannerHandoff(_ context.Context, taskID, expID int64, instance, run string) (*db.PlannerHandoffLease, *db.PlannerHandoff, error) {
	s.beginCalls++
	return &db.PlannerHandoffLease{TaskID: taskID, ExplorationID: expID, InstanceID: instance, RunID: run, Epoch: int64(s.beginCalls), ContractGeneration: 1, ContractFingerprint: s.basis.ContractFingerprint}, s.current, nil
}
func (s *handoffTestStore) GetPlannerHandoff(context.Context, int64) (*db.PlannerHandoff, error) {
	s.getCalls++
	return s.current, nil
}
func (s *handoffTestStore) CurrentPlannerHandoffBasis(context.Context, int64) (db.PlannerHandoffBasis, error) {
	return s.basis, nil
}
func (s *handoffTestStore) ReconcilePlannerHandoff(_ context.Context, _ int64, c db.PlannerHandoffContent) (db.PlannerHandoffContent, error) {
	if err := db.ValidatePlannerHandoffContent(c); err != nil {
		return c, err
	}
	raw, _ := json.Marshal(c)
	var out db.PlannerHandoffContent
	_ = json.Unmarshal(raw, &out)
	if s.invalidReferences || c.Basis.ContractFingerprint != s.basis.ContractFingerprint || c.Basis.EvidenceFingerprint != s.basis.EvidenceFingerprint {
		out.Decision = ""
		out.DeferredReason = ""
		out.ReconsiderWhen = ""
		out.NextDependencies = ""
		out.BackgroundMissing = true
		for i := range out.Todos {
			out.Todos[i].NeedsReview = true
		}
	}
	return out, nil
}
func (s *handoffTestStore) CommitPlannerHandoff(_ context.Context, lease db.PlannerHandoffLease, request string, version int64, c db.PlannerHandoffContent, generated time.Time) (*db.PlannerHandoff, error) {
	s.commitCalls++
	if s.commitErr != nil {
		return nil, s.commitErr
	}
	if !s.options.Save {
		return nil, db.ErrPlannerHandoffDisabled
	}
	if s.current != nil && s.current.Version != version {
		return nil, db.ErrPlannerHandoffConflict
	}
	hash, err := db.PlannerHandoffContentHash(c)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(c)
	var copied db.PlannerHandoffContent
	_ = json.Unmarshal(raw, &copied)
	s.current = &db.PlannerHandoff{TaskID: lease.TaskID, ExplorationID: lease.ExplorationID, Version: version + 1, RequestID: request, RunID: lease.RunID, InstanceID: lease.InstanceID, Content: copied, ContentHash: hash, GeneratedAt: generated, CommittedAt: time.Now().UTC()}
	if s.lostAcknowledgement {
		return nil, errors.New("connection lost after commit")
	}
	return s.current, nil
}
func newHandoffTestPlanner(s *handoffTestStore) *Planner {
	p := NewPlanner(nil, "fixture", "", nil, 0, 0)
	p.handoffStore = s
	return p
}
func handoffTestFixture() *handoffTestStore {
	return &handoffTestStore{options: db.PlannerHandoffOptions{Save: true, Restore: true}, basis: db.PlannerHandoffBasis{ContractFingerprint: "contract-1", EvidenceFingerprint: "graph-1"}}
}
func writeHandoffTodo(t *testing.T, s *actool.TodoStore, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"content": text, "status": "pending"}}})
	if _, err := s.Tool().Call(context.Background(), raw, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPlannerTodoIdentityDoesNotInferCompletionAcrossText(t *testing.T) {
	before := []actool.Todo{{ID: "alpha", Content: "A", Status: actool.TodoCompleted}, {ID: "beta", Content: "B", Status: actool.TodoPending}}
	tests := []struct {
		name       string
		input      []actool.Todo
		wantIDs    []string
		wantReview []bool
	}{
		{"legacy reorder", []actool.Todo{{Content: "B", Status: actool.TodoPending}, {Content: "A", Status: actool.TodoPending}}, []string{"beta", "alpha"}, []bool{false, false}},
		{"explicit rename", []actool.Todo{{ID: "alpha", Content: "A renamed", Status: actool.TodoPending}}, []string{"alpha"}, []bool{false}},
		{"legacy rename is new", []actool.Todo{{Content: "A renamed", Status: actool.TodoPending}}, []string{""}, []bool{true}},
		{"duplicate legacy text is ambiguous", []actool.Todo{{Content: "A", Status: actool.TodoPending}, {Content: "A", Status: actool.TodoPending}}, []string{"", ""}, []bool{true, true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizePlannerTodos(before, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			for i, item := range got {
				if tc.wantIDs[i] != "" && item.ID != tc.wantIDs[i] {
					t.Fatalf("id=%q want %q", item.ID, tc.wantIDs[i])
				}
				if tc.wantIDs[i] == "" && (item.ID == "" || item.ID == "alpha" || item.ID == "beta") {
					t.Fatalf("new/ambiguous item inherited old identity: %+v", item)
				}
				if item.Status != actool.TodoPending || item.NeedsReview != tc.wantReview[i] {
					t.Fatalf("completion inferred from text: %+v", item)
				}
			}
		})
	}
}

func TestPlannerHandoffRestoresOnlyConfirmedAcrossInstances(t *testing.T) {
	store := handoffTestFixture()
	first := newHandoffTestPlanner(store)
	todos := first.todoFor(8)
	round := first.beginHandoffRound(context.Background(), 5, 8, todos, nil)
	writeHandoffTodo(t, todos, "retain a dependent candidate")
	if status := first.HandoffStatus(8); status.State == "submitted" {
		t.Fatalf("memory success claimed durable commit: %+v", status)
	}
	round.save(context.Background())
	if store.current == nil || first.HandoffStatus(8).State != "submitted" {
		t.Fatal("snapshot was not committed")
	}
	id := todos.List()[0].ID
	second := newHandoffTestPlanner(store)
	restored := second.todoFor(8)
	second.beginHandoffRound(context.Background(), 5, 8, restored, nil)
	if got := restored.List(); len(got) != 1 || got[0].ID != id || got[0].Content != "retain a dependent candidate" {
		t.Fatalf("reassembled planner lost identity: %+v", got)
	}
	first.RevokeHandoff()
	round.save(context.Background())
	if store.commitCalls != 1 {
		t.Fatalf("retired instance published: %d", store.commitCalls)
	}
	if next := first.beginHandoffRound(context.Background(), 5, 8, todos, nil); next != nil {
		t.Fatal("retired pointer acquired another lease")
	}
}

func TestPlannerHandoffLostAcknowledgementUsesOneReceiptLookup(t *testing.T) {
	store := handoffTestFixture()
	store.lostAcknowledgement = true
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(9)
	round := p.beginHandoffRound(context.Background(), 5, 9, todos, nil)
	writeHandoffTodo(t, todos, "keep unresolved scope")
	round.save(context.Background())
	if store.commitCalls != 1 || store.getCalls != 1 || p.HandoffStatus(9).State != "submitted" {
		t.Fatalf("not idempotent: writes=%d reads=%d status=%+v", store.commitCalls, store.getCalls, p.HandoffStatus(9))
	}
	round.save(context.Background())
	if store.commitCalls != 1 {
		t.Fatal("unchanged content created another commit")
	}
}

func TestPlannerHandoffFailureRetainsSameInstanceMemoryAndOldSnapshot(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(11)
	first := p.beginHandoffRound(context.Background(), 5, 11, todos, nil)
	writeHandoffTodo(t, todos, "old confirmed")
	first.save(context.Background())
	oldVersion := store.current.Version
	next := p.beginHandoffRound(context.Background(), 5, 11, todos, nil)
	writeHandoffTodo(t, todos, "new uncommitted")
	store.commitErr = errors.New("PG unavailable")
	next.save(context.Background())
	if store.current.Version != oldVersion || p.HandoffStatus(11).State != "unknown" {
		t.Fatal("failure changed the durable snapshot or hid uncertainty")
	}
	again := p.beginHandoffRound(context.Background(), 5, 11, todos, nil)
	if again == nil || todos.List()[0].Content != "new uncommitted" || p.HandoffStatus(11).State == "submitted" {
		t.Fatalf("failed memory update was lost or claimed submitted: %+v %+v", todos.List(), p.HandoffStatus(11))
	}
}

func TestPlannerHandoffHeartbeatDoesNotCreateVersionOrGeneration(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(12)
	first := p.beginHandoffRound(context.Background(), 5, 12, todos, nil)
	first.save(context.Background())
	generated := store.current.GeneratedAt
	next := p.beginHandoffRound(context.Background(), 5, 12, todos, nil)
	next.save(context.Background())
	if store.commitCalls != 1 || store.current.Version != 1 || !store.current.GeneratedAt.Equal(generated) {
		t.Fatalf("heartbeat rewrote content generation: %+v", store.current)
	}
}

func TestPlannerHandoffRestoreOnlyAndInflightDisable(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(13)
	first := p.beginHandoffRound(context.Background(), 5, 13, todos, nil)
	writeHandoffTodo(t, todos, "candidate")
	first.save(context.Background())
	store.options.Save = false
	fresh := newHandoffTestPlanner(store)
	loaded := fresh.todoFor(13)
	round := fresh.beginHandoffRound(context.Background(), 5, 13, loaded, nil)
	if len(loaded.List()) != 1 || store.beginCalls != 1 {
		t.Fatal("restore-only unexpectedly acquired writer or failed reading")
	}
	round.save(context.Background())
	if store.commitCalls != 1 {
		t.Fatal("restore-only wrote snapshot")
	}
	store.options.Restore = false
	if text := round.prompt(context.Background()); strings.Contains(text, "旧模型交接参考数据") || len(loaded.List()) != 0 {
		t.Fatalf("inflight disable retained restored projection: %q %+v", text, loaded.List())
	}
	store.options.Restore = true
	fresh.beginHandoffRound(context.Background(), 5, 13, loaded, nil)
	store.errorOptions = errors.New("PG down")
	if fresh.beginHandoffRound(context.Background(), 5, 13, loaded, nil) != nil || len(loaded.List()) != 0 {
		t.Fatal("options failure injected stale restored projection")
	}
}

func TestPlannerHandoffExplicitContextRejectsCredentialsAndDoesNotUseFinalText(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	round := p.beginHandoffRound(context.Background(), 5, 14, p.todoFor(14), nil)
	tool := round.contextTool()
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"decision":"Authorization: Bearer secret"}`), nil); err == nil {
		t.Fatal("credential copy accepted")
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"decision":"wait for the existing worker","next_dependencies":"fact from current check"}`), nil); err != nil {
		t.Fatal(err)
	}
	round.save(context.Background())
	if store.current.Content.Decision != "wait for the existing worker" || store.current.Content.BackgroundMissing {
		t.Fatal("explicit background missing")
	}
	// An unrequested final answer/thinking never reaches this explicit channel.
	if strings.Contains(round.prompt(context.Background()), "planner_handoff_context") {
		t.Fatal("prompt requests a tool not permitted by final tool resolution")
	}
	round.contextAllowed = true
	if !strings.Contains(round.prompt(context.Background()), "planner_handoff_context") {
		t.Fatal("enabled context tool missing guidance")
	}
}

func TestPlannerHandoffStaleReferenceIsDegradedWithoutCompletionMutation(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(15)
	first := p.beginHandoffRound(context.Background(), 5, 15, todos, nil)
	writeHandoffTodo(t, todos, "pending review")
	first.save(context.Background())
	store.invalidReferences = true
	next := newHandoffTestPlanner(store)
	loaded := next.todoFor(15)
	next.beginHandoffRound(context.Background(), 5, 15, loaded, nil)
	if status := next.HandoffStatus(15); status.RecoveryState != "degraded" {
		t.Fatalf("invalid refs restored as current: %+v", status)
	}
	if got := loaded.List()[0]; !got.NeedsReview || got.Status != actool.TodoPending {
		t.Fatalf("projection rewrote completion: %+v", got)
	}
}

func TestPlannerHandoffCancelledSaveAndBounds(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(16)
	round := p.beginHandoffRound(context.Background(), 5, 16, todos, nil)
	writeHandoffTodo(t, todos, "pending")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	round.save(ctx)
	if store.commitCalls != 0 || p.HandoffStatus(16).State != "failed" {
		t.Fatal("cancelled commit was reported successful")
	}
	entries := make([]actool.Todo, db.MaxPlannerHandoffTodos+1)
	for i := range entries {
		entries[i] = actool.Todo{Content: fmt.Sprintf("item %d", i), Status: actool.TodoPending}
	}
	if _, err := normalizePlannerTodos(nil, entries); !errors.Is(err, db.ErrPlannerHandoffTooLarge) {
		t.Fatal("oversized list silently truncated")
	}
}

func TestPlannerHandoffTodoRevisionPreservesBackgroundGeneration(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(17)
	first := p.beginHandoffRound(context.Background(), 5, 17, todos, nil)
	if _, err := first.contextTool().Call(context.Background(), json.RawMessage(`{"decision":"wait for existing check"}`), nil); err != nil {
		t.Fatal(err)
	}
	first.save(context.Background())
	// Use a historical persisted stamp rather than relying on wall-clock tick
	// resolution between two immediate writes (coarse on some Windows hosts).
	historical := time.Now().UTC().Add(-time.Hour)
	store.current.GeneratedAt = historical
	store.current.Content.BackgroundGeneratedAt = &historical
	store.current.ContentHash, _ = db.PlannerHandoffContentHash(store.current.Content)
	oldContentGenerated := store.current.GeneratedAt
	oldBackgroundGenerated := *store.current.Content.BackgroundGeneratedAt
	next := p.beginHandoffRound(context.Background(), 5, 17, todos, nil)
	writeHandoffTodo(t, todos, "new candidate")
	next.save(context.Background())
	if !store.current.GeneratedAt.After(oldContentGenerated) {
		t.Fatal("todo revision retained old content generation time")
	}
	if store.current.Content.BackgroundGeneratedAt == nil || !store.current.Content.BackgroundGeneratedAt.Equal(oldBackgroundGenerated) {
		t.Fatal("todo revision disguised old background as newly generated")
	}
	version := store.current.Version
	if _, err := next.contextTool().Call(context.Background(), json.RawMessage(`{"decision":"wait for existing check"}`), nil); err != nil {
		t.Fatal(err)
	}
	next.save(context.Background())
	if store.current.Version != version {
		t.Fatal("identical background created a version")
	}
}

func TestPlannerHandoffConflictRequiresExplicitRevision(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(18)
	first := p.beginHandoffRound(context.Background(), 5, 18, todos, nil)
	writeHandoffTodo(t, todos, "confirmed")
	first.save(context.Background())
	next := p.beginHandoffRound(context.Background(), 5, 18, todos, nil)
	writeHandoffTodo(t, todos, "local dirty")
	store.commitErr = errors.New("PG failure")
	next.save(context.Background())
	store.commitErr = nil
	other := newHandoffTestPlanner(store)
	otherTodos := other.todoFor(18)
	otherRound := other.beginHandoffRound(context.Background(), 5, 18, otherTodos, nil)
	writeHandoffTodo(t, otherTodos, "other committed")
	otherRound.save(context.Background())
	count := store.commitCalls
	conflicted := p.beginHandoffRound(context.Background(), 5, 18, todos, nil)
	conflicted.save(context.Background())
	if store.commitCalls != count || p.HandoffStatus(18).State != "failed" || todos.List()[0].Content != "local dirty" {
		t.Fatal("conflict automatically overwrote current snapshot or discarded local memory")
	}
	writeHandoffTodo(t, todos, "explicitly reviewed revision")
	conflicted.save(context.Background())
	if store.current.Content.Todos[0].Content != "explicitly reviewed revision" {
		t.Fatal("current instance could not commit explicit revision")
	}
}

func TestPlannerHandoffToolIsManagedAndPlannerOnly(t *testing.T) {
	found := false
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == plannerHandoffContextTool {
			found = true
			if len(seed.Agents) != 1 || seed.Agents[0] != "planner" {
				t.Fatalf("unexpected bindings: %+v", seed.Agents)
			}
		}
	}
	if !found {
		t.Fatal("handoff context tool bypasses managed catalog")
	}
}

func TestPlannerHandoffPostgresPlanAndReassembly(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	ctx := context.Background()
	on := true
	off := false
	if err := d.SetPlannerHandoffSettings(ctx, &on, &on); err != nil {
		t.Fatal(err)
	}
	defer d.SetPlannerHandoffSettings(ctx, &off, &off)
	task, err := d.CreateTask("handoff fixture", "retain checks and dependencies", nil, 0, 600)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if err = d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(task.ExplorationID)
	calls := 0
	provider := completionReviewProvider{stream: func(_ context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		calls++
		switch calls {
		case 1:
			found := false
			for _, tool := range req.Tools {
				found = found || tool.Name == plannerHandoffContextTool
			}
			if !found {
				t.Error("handoff context tool was not exposed")
			}
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "todo", ToolName: "TodoWrite"}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"todos":[{"content":"wait for evidence then verify candidate","status":"pending"}]}`}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
		case 2:
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "background", ToolName: plannerHandoffContextTool}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"decision":"retain candidate while existing work supplies evidence","next_dependencies":"current check result"}`}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"})
		default:
			completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "ordinary final text is not a second handoff"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
		}
	}}
	first := NewPlanner(provider, "fixture", t.TempDir(), nil, 0, 10)
	first.SetHandoffStore(d)
	if err = d.RegisterPlannerHandoffInstance(ctx, task.ID, task.ExplorationID, first.HandoffInstanceID()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = first.Plan(ctx, task.ID, nil, store, task.Goal, nil, nil); err != nil {
		t.Fatal(err)
	}
	committed, err := d.GetPlannerHandoff(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed == nil || len(committed.Content.Todos) != 1 || committed.Content.Decision != "retain candidate while existing work supplies evidence" || committed.Content.BackgroundGeneratedAt == nil {
		t.Fatalf("incomplete committed content: %+v", committed)
	}
	if calls != 3 {
		t.Fatalf("hidden summary model call: %d", calls)
	}
	id := committed.Content.Todos[0].ID
	recoveredPrompt := ""
	resumedProvider := completionReviewProvider{stream: func(_ context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) {
		recoveredPrompt = req.Messages[len(req.Messages)-1].Text()
		completionReviewEvents(yield, llm.StreamEvent{Type: llm.SETextDelta, Text: "no changes"}, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"})
	}}
	next := NewPlanner(resumedProvider, "fixture", t.TempDir(), nil, 0, 5)
	next.SetHandoffStore(d)
	first.RevokeHandoff()
	if err = d.RegisterPlannerHandoffInstance(ctx, task.ID, task.ExplorationID, next.HandoffInstanceID()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = next.Plan(ctx, task.ID, nil, store, task.Goal, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recoveredPrompt, id) || !strings.Contains(recoveredPrompt, "retain candidate while existing work supplies evidence") {
		t.Fatalf("reassembly failed to inject committed background/todo: %s", recoveredPrompt)
	}
	latest, err := d.GetPlannerHandoff(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != committed.Version {
		t.Fatalf("no-change round added version: %d -> %d", committed.Version, latest.Version)
	}
	if !latest.CheckedAt.After(committed.CheckedAt) || !latest.GeneratedAt.Equal(committed.GeneratedAt) {
		t.Fatal("effective no-change round must update only last checked time")
	}
}

func TestPlannerHandoffPostgresGraphCommitFailureAndEvidenceSettlement(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	ctx := context.Background()
	on := true
	off := false
	if err := d.SetPlannerHandoffSettings(ctx, &on, &on); err != nil {
		t.Fatal(err)
	}
	defer d.SetPlannerHandoffSettings(ctx, &off, &off)
	task, err := d.CreateTask("handoff lifecycle fixture", "preserve evidence", nil, 0, 600)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if err = d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	graph := d.Exploration(task.ExplorationID)
	p := NewPlanner(nil, "fixture", t.TempDir(), nil, 0, 0)
	p.SetHandoffStore(d)
	if err = d.RegisterPlannerHandoffInstance(ctx, task.ID, task.ExplorationID, p.HandoffInstanceID()); err != nil {
		t.Fatal(err)
	}
	round := p.beginHandoffRound(ctx, task.ID, task.ExplorationID, p.todoFor(task.ExplorationID), nil)
	if round == nil {
		t.Fatal("no handoff round")
	}
	fact, err := graph.AddNode(db.KindFact, map[string]any{"summary": "already committed evidence", "confidence": "observed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeHandoffTodo(t, round.todos, "preserve unfinished check")
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	round.save(ctx)
	if status := p.HandoffStatus(task.ExplorationID); status.State != "failed" {
		t.Fatalf("late paused save succeeded: %+v", status)
	}
	saved, err := graph.GetNode(fact)
	if err != nil || saved == nil {
		t.Fatal("handoff failure rolled back committed business graph")
	}
	// The handoff publishing fence must not become a new execution/evidence fence.
	tail, err := graph.AddNode(db.KindFact, map[string]any{"summary": "allowed existing evidence settlement"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatalf("handoff gate blocked original evidence saving: %v", err)
	}
	if saved, err = graph.GetNode(tail); err != nil || saved == nil {
		t.Fatal("paused evidence settlement missing")
	}
	if committed, err := d.GetPlannerHandoff(ctx, task.ID); err != nil || committed != nil {
		t.Fatal("paused snapshot was rebuilt")
	}
}

func TestPlannerHandoffSaveOnlyKeepsOwnMemoryWithoutRestoringOtherInstance(t *testing.T) {
	store := handoffTestFixture()
	store.options.Restore = false
	p := newHandoffTestPlanner(store)
	todos := p.todoFor(19)
	first := p.beginHandoffRound(context.Background(), 5, 19, todos, nil)
	writeHandoffTodo(t, todos, "same instance todo")
	if _, err := first.contextTool().Call(context.Background(), json.RawMessage(`{"decision":"explicit same-instance background"}`), nil); err != nil {
		t.Fatal(err)
	}
	first.save(context.Background())
	version := store.current.Version
	next := p.beginHandoffRound(context.Background(), 5, 19, todos, nil)
	next.save(context.Background())
	if store.current.Version != version || store.current.Content.Decision != "explicit same-instance background" {
		t.Fatal("save-only lost own memory or rewrote unchanged snapshot")
	}
	if strings.Contains(next.prompt(context.Background()), "旧模型交接参考数据") {
		t.Fatal("save-only injected old background")
	}
	fresh := newHandoffTestPlanner(store)
	freshTodos := fresh.todoFor(19)
	freshRound := fresh.beginHandoffRound(context.Background(), 5, 19, freshTodos, nil)
	freshRound.finalizing = true
	freshRound.save(context.Background())
	if store.current.Version != version || store.current.Content.Decision != "explicit same-instance background" {
		t.Fatal("restore-disabled empty heartbeat erased committed snapshot")
	}
	if len(freshTodos.List()) != 0 {
		t.Fatal("restore-disabled new instance loaded PG todo")
	}
	if _, err := freshTodos.Tool().Call(context.Background(), json.RawMessage(`{"todos":[]}`), nil); err != nil {
		t.Fatal(err)
	}
	freshRound.save(context.Background())
	if store.current.Version != version+1 || len(store.current.Content.Todos) != 0 {
		t.Fatal("explicit whole-list deletion was ignored")
	}
}

func TestPlannerHandoffRevokedLateSaveAuditsOnceWithoutPublication(t *testing.T) {
	store := handoffTestFixture()
	p := newHandoffTestPlanner(store)
	var audit []db.Activity
	round := p.beginHandoffRound(context.Background(), 5, 20, p.todoFor(20), func(a db.Activity) { audit = append(audit, a) })
	writeHandoffTodo(t, round.todos, "unfinished check")
	p.RevokeHandoff()
	round.save(context.Background())
	round.save(context.Background())
	if store.commitCalls != 0 || store.current != nil {
		t.Fatal("revoked callback published a snapshot")
	}
	if len(audit) != 1 || !audit[0].IsError || !strings.Contains(audit[0].Detail, "发布资格已撤销") || !strings.Contains(audit[0].Detail, "证据收尾不受影响") {
		t.Fatalf("missing or repeated rejection audit: %+v", audit)
	}
	if p.HandoffStatus(20).State != "failed" {
		t.Fatal("rejected late save was not visible")
	}
}
