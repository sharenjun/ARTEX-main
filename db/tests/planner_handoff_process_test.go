//go:build ignore

package db

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const handoffProcessReportPrefix = "ARTEX_HANDOFF_PROCESS_REPORT="

type handoffProcessReport struct {
	PID         int                   `json:"pid"`
	ReadOnly    bool                  `json:"read_only"`
	Current     *PlannerHandoff       `json:"current"`
	History     []PlannerHandoff      `json:"history"`
	Projection  PlannerHandoffContent `json:"projection"`
	NodeState   string                `json:"node_state"`
	NodePayload json.RawMessage       `json:"node_payload"`
	Publisher   string                `json:"publisher"`
	Epoch       int64                 `json:"epoch"`
}

// This helper is launched as a fresh OS process. It deliberately opens only a
// read-only SQL handle: DB.Open would also migrate/seed, obscuring whether the
// recovery read itself changed business data or publishing eligibility.
func TestPlannerHandoffProcessHelper(t *testing.T) {
	if os.Getenv("ARTEX_HANDOFF_PROCESS_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	taskID, err := strconv.ParseInt(os.Getenv("ARTEX_HANDOFF_PROCESS_TASK_ID"), 10, 64)
	if err != nil || taskID <= 0 {
		t.Fatal("invalid process fixture task identity")
	}
	nodeID, err := strconv.ParseInt(os.Getenv("ARTEX_HANDOFF_PROCESS_NODE_ID"), 10, 64)
	if err != nil || nodeID <= 0 {
		t.Fatal("invalid process fixture node identity")
	}
	dsn, err := url.Parse(os.Getenv("ARTEX_HANDOFF_PROCESS_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	params := dsn.Query()
	params.Set("default_transaction_read_only", "on")
	dsn.RawQuery = params.Encode()
	sqlDB, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	d := &DB{sqlDB}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	var readOnly string
	if err := sqlDB.QueryRowContext(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
		t.Fatal(err)
	}
	report := handoffProcessReport{PID: os.Getpid(), ReadOnly: readOnly == "on"}
	if !report.ReadOnly {
		t.Fatal("child recovery connection is not read-only")
	}
	report.Current, err = d.GetPlannerHandoff(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current == nil {
		t.Fatal("child did not recover committed handoff")
	}
	report.History, err = d.PlannerHandoffHistory(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	report.Projection, err = d.ReconcilePlannerHandoff(ctx, taskID, report.Current.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT state,payload FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, nodeID, report.Current.ExplorationID).Scan(&report.NodeState, &report.NodePayload); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT planner_handoff_instance_id,planner_handoff_epoch FROM tasks WHERE id=$1`, taskID).Scan(&report.Publisher, &report.Epoch); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s%s\n", handoffProcessReportPrefix, raw)
}

func TestPlannerHandoffCommittedSnapshotSurvivesNewProcess(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("ARTEX_PG_DSN must point to an isolated disposable PostgreSQL database")
	}
	d, task := handoffFixture(t)
	store := d.Exploration(task.ExplorationID)
	intentID, err := store.AddCheckIntent(t.Context(), map[string]any{"summary": "existing uncompleted evidence check"}, 1, nil, "planner", CheckSpec{Target: "local process fixture authentication", Mechanism: "authentication"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{
		Result: "blocked", Scope: []string{"two authorized candidate references"},
		Evidence: "candidate a rejected; candidate b has not been attempted",
		Gaps:     []string{"candidate b remains unverified"}, Barrier: "wait for current task condition",
		NextAction: "Read saved evidence before any further action",
		Auth:       &AuthCoverage{Request: "POST /fixture/login JSON candidate reference", Budget: 2, Candidates: []AuthCandidate{{ID: "candidate-a", State: "failed", Attempts: 1}, {ID: "candidate-b", State: "remaining"}}},
	}
	if _, err := store.RecordCheckFact(t.Context(), map[string]any{"summary": "bounded partial result"}, intentID, checkpoint, "worker", nil); err != nil {
		t.Fatal(err)
	}
	content := handoffContent(t, d, task.ID)
	content.Todos = []PlannerHandoffTodo{
		{ID: "stable-pending", Content: "Confirm an unverified dependency", Status: "pending"},
		{ID: "stable-progress", Content: "Keep the existing check open", Status: "in_progress", References: []PlannerHandoffReference{{Kind: "node", ID: intentID, Available: true}}},
		{ID: "stable-old-complete", Content: "Previously completed planning note", Status: "completed"},
	}
	// The reference is an actual read of the existing intent version. Recovery
	// must preserve that identity and the checkpoint budget, never reopen work.
	if err := d.QueryRow(`SELECT content_version FROM exploration_nodes WHERE id=$1`, intentID).Scan(&content.Todos[1].References[0].Version); err != nil {
		t.Fatal(err)
	}
	lease, _ := beginHandoff(t, d, task, "parent-process-round")
	firstAt := time.Now().UTC().Truncate(time.Microsecond)
	first, err := d.CommitPlannerHandoff(t.Context(), *lease, "parent-process-request-1", 0, content, firstAt)
	if err != nil {
		t.Fatal(err)
	}
	content.DeferredReason = "Waiting on the already running bounded check."
	content.NextDependencies = "Read its saved result before deciding any next step."
	committedAt := time.Now().UTC().Truncate(time.Microsecond)
	saved, err := d.CommitPlannerHandoff(t.Context(), *lease, "parent-process-request-2", first.Version, content, committedAt)
	if err != nil {
		t.Fatal(err)
	}
	var originalPayload json.RawMessage
	if err := d.QueryRow(`SELECT payload FROM exploration_nodes WHERE id=$1`, intentID).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	var beforeCount int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, task.ExplorationID).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	// This in-memory edit is intentionally never submitted. Nothing is passed
	// to the child except stable task/node IDs and the isolated database DSN.
	content.Todos[0].Content = "unsubmitted parent memory must not appear"
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlannerHandoffProcessHelper$", "-test.count=1")
	// TestMain normally serializes the whole database suite through an
	// advisory lock held by the parent. Give that bootstrap a syntactically
	// invalid DSN so it takes its existing no-connection branch immediately;
	// the helper receives the isolated DSN separately and only opens it read-only.
	childEnv := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ARTEX_PG_DSN=") {
			childEnv = append(childEnv, entry)
		}
	}
	command.Env = append(childEnv, "ARTEX_PG_DSN=postgres://invalid/%", "ARTEX_HANDOFF_PROCESS_DSN="+os.Getenv("ARTEX_PG_DSN"), "ARTEX_HANDOFF_PROCESS_HELPER=1", "ARTEX_HANDOFF_PROCESS_TASK_ID="+strconv.FormatInt(task.ID, 10), "ARTEX_HANDOFF_PROCESS_NODE_ID="+strconv.FormatInt(intentID, 10))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("child recovery failed: %v\n%s", err, output)
	}
	var report handoffProcessReport
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), MaxPlannerHandoffBytes*(PlannerHandoffHistoryLimit+3))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, handoffProcessReportPrefix) {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, handoffProcessReportPrefix)), &report); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("child produced no recovery report: %s", output)
	}
	if report.PID == os.Getpid() || report.PID <= 0 || !report.ReadOnly {
		t.Fatalf("recovery did not run in an isolated read-only process: pid=%d readonly=%v", report.PID, report.ReadOnly)
	}
	if report.Current == nil || report.Current.Version != saved.Version || report.Current.ContentHash != saved.ContentHash || report.Current.RequestID != saved.RequestID {
		t.Fatalf("committed identity changed across process: %+v", report.Current)
	}
	if !report.Current.GeneratedAt.Equal(saved.GeneratedAt) || !report.Current.CommittedAt.Equal(saved.CommittedAt) || !report.Current.CheckedAt.Equal(saved.CheckedAt) {
		t.Fatal("new process fabricated generation/commit/check times")
	}
	if len(report.History) != 2 || report.History[0].Version != 2 || report.History[1].Version != 1 {
		t.Fatalf("history did not survive process boundary: %+v", report.History)
	}
	if len(report.Projection.Todos) != 3 || report.Projection.Todos[0].ID != "stable-pending" || report.Projection.Todos[0].Status != "pending" || report.Projection.Todos[1].ID != "stable-progress" || report.Projection.Todos[1].Status != "in_progress" {
		t.Fatalf("uncompleted stable todos were lost: %+v", report.Projection.Todos)
	}
	if report.Projection.Todos[0].Content == content.Todos[0].Content {
		t.Fatal("new process read an unsubmitted in-memory edit")
	}
	var recoveredJSON, originalJSON bytes.Buffer
	if err := json.Compact(&recoveredJSON, report.NodePayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&originalJSON, originalPayload); err != nil {
		t.Fatal(err)
	}
	if report.NodeState != "open" || !bytes.Equal(recoveredJSON.Bytes(), originalJSON.Bytes()) {
		t.Fatalf("recovery changed work/checkpoint state: %s %s", report.NodeState, report.NodePayload)
	}
	var recoveredCheck struct {
		Checkpoint Checkpoint `json:"checkpoint"`
		Attempt    int        `json:"check_attempt"`
	}
	if err := json.Unmarshal(report.NodePayload, &recoveredCheck); err != nil {
		t.Fatal(err)
	}
	if recoveredCheck.Checkpoint.Auth == nil || recoveredCheck.Checkpoint.Auth.Budget != 2 || recoveredCheck.Checkpoint.Auth.Candidates[0].Attempts != 1 || recoveredCheck.Checkpoint.Auth.Candidates[1].State != "remaining" || recoveredCheck.Attempt != 1 {
		t.Fatalf("cross-process recovery reset checkpoint budget/candidates: %+v", recoveredCheck)
	}
	if report.Publisher != lease.InstanceID || report.Epoch != lease.Epoch {
		t.Fatalf("read-only recovery changed publishing eligibility: publisher=%q epoch=%d", report.Publisher, report.Epoch)
	}
	after, err := d.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != saved.Version || after.ContentHash != saved.ContentHash || !after.CheckedAt.Equal(saved.CheckedAt) {
		t.Fatal("recovery read created a new version or confirmation")
	}
	var afterCount int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, task.ExplorationID).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if beforeCount != afterCount {
		t.Fatalf("recovery replayed activity: before=%d after=%d", beforeCount, afterCount)
	}
}
