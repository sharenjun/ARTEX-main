//go:build ignore

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func handoffFixture(t *testing.T) (*DB, *Task) {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN is required for isolated Planner handoff integration tests")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	old, err := d.ReadPlannerHandoffOptions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := d.SetPlannerHandoffSettings(ctx, &old.Save, &old.Restore); err != nil {
			t.Errorf("restore handoff switches: %v", err)
		}
	})
	on := true
	if err = d.SetPlannerHandoffSettings(t.Context(), &on, &on); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("handoff fixture "+t.Name(), "current authorized task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Errorf("delete fixture: %v", err)
		}
	})
	if err = d.RegisterPlannerHandoffInstance(t.Context(), task.ID, task.ExplorationID, "instance-a"); err != nil {
		t.Fatal(err)
	}
	return d, task
}

func handoffContent(t *testing.T, d *DB, taskID int64) PlannerHandoffContent {
	t.Helper()
	basis, err := d.CurrentPlannerHandoffBasis(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return PlannerHandoffContent{FormatVersion: PlannerHandoffFormatVersion, Todos: []PlannerHandoffTodo{{ID: "todo-a", Content: "check pending authorization prerequisite", Status: "pending"}}, Decision: "Use the saved prerequisite before new work", Basis: basis}
}

func beginHandoff(t *testing.T, d *DB, task *Task, run string) (*PlannerHandoffLease, *PlannerHandoff) {
	t.Helper()
	lease, current, err := d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "instance-a", run)
	if err != nil {
		t.Fatal(err)
	}
	return lease, current
}

func TestPlannerHandoffCommitRestartNoopAndIdempotency(t *testing.T) {
	d, task := handoffFixture(t)
	content := handoffContent(t, d, task.ID)
	lease, current := beginHandoff(t, d, task, "round-a")
	if current != nil {
		t.Fatal("legacy task unexpectedly has a handoff")
	}
	at := time.Now().UTC()
	first, err := d.CommitPlannerHandoff(t.Context(), *lease, "request-a", 0, content, at)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.RequestID != "request-a" || !first.GeneratedAt.Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("bad first receipt %+v", first)
	}
	// An independent process handle reads only the committed snapshot.
	otherSQL, err := sql.Open("pgx", os.Getenv("ARTEX_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer otherSQL.Close()
	other := &DB{otherSQL}
	read, err := other.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil || read == nil || read.Content.Todos[0].ID != "todo-a" || read.Version != 1 {
		t.Fatalf("independent restore %+v %v", read, err)
	}
	// Replaying a receipt after losing its acknowledgement cannot add a version.
	again, err := other.CommitPlannerHandoff(t.Context(), *lease, "request-a", 0, content, at)
	if err != nil || again.Version != 1 {
		t.Fatalf("idempotent request %+v %v", again, err)
	}
	lease, _ = beginHandoff(t, d, task, "round-b")
	same, err := d.CommitPlannerHandoff(t.Context(), *lease, "heartbeat-b", 1, content, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if same.Version != 1 || !same.GeneratedAt.Equal(at.Truncate(time.Microsecond)) || !same.CommittedAt.Equal(first.CommittedAt) {
		t.Fatalf("heartbeat changed content identity %+v", same)
	}
	history, err := d.PlannerHandoffHistory(t.Context(), task.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("heartbeat history %d %v", len(history), err)
	}
	content.Todos[0].Content = "new explicitly revised todo"
	second, err := d.CommitPlannerHandoff(t.Context(), *lease, "request-b", 1, content, time.Now())
	if err != nil || second.Version != 2 {
		t.Fatalf("revision %+v %v", second, err)
	}
	conflicting := content
	conflicting.Decision = "different decision"
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "request-b", 1, conflicting, time.Now()); !errors.Is(err, ErrPlannerHandoffConflict) {
		t.Fatalf("request identity reused: %v", err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "bad-cas", 1, content, time.Now()); !errors.Is(err, ErrPlannerHandoffConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
}

func TestPlannerHandoffRevokesOldPublishersAndOffOn(t *testing.T) {
	d, task := handoffFixture(t)
	content := handoffContent(t, d, task.ID)
	lease, _ := beginHandoff(t, d, task, "before-reassembly")
	if err := d.RegisterPlannerHandoffInstance(t.Context(), task.ID, task.ExplorationID, "instance-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "instance-a", "late-old-round"); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("old instance reacquired publication: %v", err)
	}
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "late-old-result", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("old callback saved: %v", err)
	}
	lease, _, err := d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "instance-b", "before-disable")
	if err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	if err = d.SetPlannerHandoffSettings(t.Context(), &off, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "disabled", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffDisabled) {
		t.Fatalf("disabled save: %v", err)
	}
	if err = d.SetBool(PlannerHandoffSaveSetting, on); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "reenabled-old", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("off/on revived lease: %v", err)
	}
	lease, _, err = d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "instance-b", "current-round")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "after-pause-resume", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("pause/resume revived lease: %v", err)
	}
	if err = d.InvalidateAllPlannerHandoffInstances(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "instance-b", "after-global-reassembly"); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("invalidated publisher returned: %v", err)
	}
}

func TestPlannerHandoffContractABAAndProjectionOnlyReconcile(t *testing.T) {
	d, task := handoffFixture(t)
	store := d.Exploration(task.ExplorationID)
	goalID, err := store.AddGoal(map[string]any{"text": "stable goal", "goal_type": "inspection", "required_checks": []string{"authorization"}}, "human")
	if err != nil {
		t.Fatal(err)
	}
	content := handoffContent(t, d, task.ID)
	content.Todos[0].References = append([]PlannerHandoffReference(nil), content.Basis.References...)
	lease, _ := beginHandoff(t, d, task, "contract-read")
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "saved-before-fact", 0, content, time.Now()); err != nil {
		t.Fatal(err)
	}
	factID, err := store.AddNode("fact", map[string]any{"summary": "new evidence that requires re-evaluation"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := d.ReconcilePlannerHandoff(t.Context(), task.ID, content)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Decision != "" || !projected.BackgroundMissing || !projected.Todos[0].NeedsReview {
		t.Fatalf("stale background injected %+v", projected)
	}
	var state string
	if err = d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, factID).Scan(&state); err != nil || state != "confirmed" {
		t.Fatalf("reconcile changed business state %s %v", state, err)
	}
	original, err := d.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil || original.Content.Decision == "" {
		t.Fatalf("reconcile rewrote committed content %+v %v", original, err)
	}
	fresh := handoffContent(t, d, task.ID)
	lease, _ = beginHandoff(t, d, task, "before-contract-aba")
	cid, err := store.AddConstraint("deny", "no stateful retries", "human")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteConstraint(cid); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "late-contract-aba", 1, fresh, time.Now()); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("ABA contract accepted: %v", err)
	}
	if err = store.DeleteGoal(goalID); err != nil {
		t.Fatal(err)
	}
	projected, err = d.ReconcilePlannerHandoff(t.Context(), task.ID, content)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Todos[0].References[0].Available || projected.Todos[0].References[0].Reason == "" {
		t.Fatalf("deleted reference rebound %+v", projected.Todos[0])
	}
}

func TestPlannerHandoffRetentionCancellationAndCorruption(t *testing.T) {
	d, task := handoffFixture(t)
	content := handoffContent(t, d, task.ID)
	lease, _ := beginHandoff(t, d, task, "retained-round")
	var current *PlannerHandoff
	for i := 0; i < PlannerHandoffHistoryLimit+2; i++ {
		if i == MaxPlannerHandoffCommitsPerRun {
			content.Todos[0].Content = "extra write must be rejected"
			if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "over-round-limit", int64(i), content, time.Now()); !errors.Is(err, ErrPlannerHandoffTooLarge) {
				t.Fatalf("round receipt retention limit not enforced: %v", err)
			}
			lease, _ = beginHandoff(t, d, task, "next-retained-round")
		}
		content.Todos[0].Content = fmt.Sprintf("todo revision %d", i)
		var err error
		current, err = d.CommitPlannerHandoff(t.Context(), *lease, fmt.Sprintf("bounded-%d", i), int64(i), content, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	history, err := d.PlannerHandoffHistory(t.Context(), task.ID)
	if err != nil || len(history) != PlannerHandoffHistoryLimit || history[0].Version != current.Version {
		t.Fatalf("history retained %d %v", len(history), err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	content.Todos[0].Content = "should not publish after cancel"
	if _, err = d.CommitPlannerHandoff(canceled, *lease, "canceled", current.Version, content, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save %v", err)
	}
	saved, err := d.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil || saved.Version != current.Version {
		t.Fatalf("cancel changed current %+v %v", saved, err)
	}
	if _, err = d.Exec(`UPDATE planner_handoffs SET content='{"format_version":999}' WHERE task_id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.GetPlannerHandoff(t.Context(), task.ID); err == nil {
		t.Fatal("corrupt/incompatible handoff accepted")
	}
}

func TestPlannerHandoffDeleteAndArchiveCannotRecreateProjection(t *testing.T) {
	d, task := handoffFixture(t)
	content := handoffContent(t, d, task.ID)
	lease, _ := beginHandoff(t, d, task, "before-delete")
	if err := d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "deleted-late", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffParentGone) {
		t.Fatalf("deleted task resurrected: %v", err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM planner_handoffs WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphaned current %d %v", count, err)
	}
	second, err := d.CreateTask("archive lease", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(second.ID)
	if err = d.RegisterPlannerHandoffInstance(t.Context(), second.ID, second.ExplorationID, "instance-a"); err != nil {
		t.Fatal(err)
	}
	lease, _ = beginHandoff(t, d, second, "before-archive")
	content = handoffContent(t, d, second.ID)
	if err = d.SetPaused(second.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchive(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CommitPlannerHandoff(t.Context(), *lease, "archive-late", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffParentGone) {
		t.Fatalf("archive accepted callback: %v", err)
	}
}

func TestPlannerHandoffContentBoundsAndReferenceKinds(t *testing.T) {
	content := PlannerHandoffContent{FormatVersion: 1, Todos: []PlannerHandoffTodo{{ID: "a", Content: "work", Status: "pending"}}, BackgroundMissing: true}
	if err := ValidatePlannerHandoffContent(content); err != nil {
		t.Fatal(err)
	}
	duplicate := content
	duplicate.Todos = append(append([]PlannerHandoffTodo{}, content.Todos...), content.Todos[0])
	if err := ValidatePlannerHandoffContent(duplicate); err == nil {
		t.Fatal("duplicate todo ID accepted")
	}
	large := content
	large.Decision = strings.Repeat("x", 4097)
	if err := ValidatePlannerHandoffContent(large); !errors.Is(err, ErrPlannerHandoffTooLarge) {
		t.Fatalf("oversized background %v", err)
	}
	aggregate := content
	aggregate.Todos = nil
	for i := 0; i < MaxPlannerHandoffTodos; i++ {
		aggregate.Todos = append(aggregate.Todos, PlannerHandoffTodo{ID: fmt.Sprint(i), Content: strings.Repeat("x", 2048), Status: "pending"})
	}
	if err := ValidatePlannerHandoffContent(aggregate); !errors.Is(err, ErrPlannerHandoffTooLarge) {
		t.Fatalf("aggregate canonical size limit not enforced: %v", err)
	}
	invalid := content
	invalid.Todos = []PlannerHandoffTodo{{ID: "a", Content: "work", Status: "pending", References: []PlannerHandoffReference{{Kind: "asset", ID: 1}}}}
	if err := ValidatePlannerHandoffContent(invalid); err == nil {
		t.Fatal("unimplemented asset reference accepted")
	}
	if plannerHandoffGate == schemaMigrationLockKey || plannerHandoffGate == 7337741002 {
		t.Fatal("handoff gate reuses migration or suite lock")
	}
}

func TestPlannerHandoffConcurrentContractMutationIsSerialized(t *testing.T) {
	for _, kind := range []string{"goal", "scope"} {
		t.Run(kind, func(t *testing.T) {
			d, task := handoffFixture(t)
			content := handoffContent(t, d, task.ID)
			lease, _ := beginHandoff(t, d, task, "concurrent-contract")
			start := make(chan struct{})
			committed := make(chan error, 1)
			mutated := make(chan error, 1)
			go func() {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, err := d.CommitPlannerHandoff(ctx, *lease, "concurrent-request", 0, content, time.Now())
				committed <- err
			}()
			go func() {
				<-start
				var err error
				if kind == "goal" {
					_, err = d.Exploration(task.ExplorationID).AddGoal(map[string]any{"text": "new manual goal"}, "human")
				} else {
					_, err = d.Assets().RegisterTaskAssetScopes(task.ID, []ScopeInput{{Kind: "domain", Value: "handoff-concurrency.invalid"}})
				}
				mutated <- err
			}()
			close(start)
			if err := <-mutated; err != nil {
				t.Fatalf("contract mutation failed: %v", err)
			}
			if err := <-committed; err != nil && !errors.Is(err, ErrPlannerHandoffStale) {
				t.Fatalf("unexpected concurrent result/deadlock: %v", err)
			}
			// Whichever operation won first, this old token cannot publish after
			// the authorized mutation has completed.
			if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "post-mutation-stale", 0, content, time.Now()); !errors.Is(err, ErrPlannerHandoffStale) {
				t.Fatalf("old publication survived contract mutation: %v", err)
			}
		})
	}
}

func TestPlannerHandoffLargeCanonicalContentRoundTripsJSONB(t *testing.T) {
	d, task := handoffFixture(t)
	content := handoffContent(t, d, task.ID)
	content.Todos = nil
	for i := 0; i < 30; i++ {
		content.Todos = append(content.Todos, PlannerHandoffTodo{ID: fmt.Sprint(i), Content: strings.Repeat("x", 2048), Status: "pending"})
	}
	// Fit the canonical 64KiB cap while PostgreSQL jsonb adds enough whitespace
	// that its own textual encoding crosses the same threshold.
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	remaining := MaxPlannerHandoffBytes - len(raw) - 64
	if remaining < 0 || remaining > 4096 {
		t.Fatalf("unexpected fixture spare bytes %d", remaining)
	}
	content.Decision = strings.Repeat("y", remaining)
	if err = ValidatePlannerHandoffContent(content); err != nil {
		t.Fatal(err)
	}
	lease, _ := beginHandoff(t, d, task, "large-jsonb")
	committed, err := d.CommitPlannerHandoff(t.Context(), *lease, "large-request", 0, content, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	read, err := d.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil || read == nil || read.ContentHash != committed.ContentHash {
		t.Fatalf("jsonb canonical content failed %+v %v", read, err)
	}
	var storedBytes int
	if err = d.QueryRow(`SELECT octet_length(content::text) FROM planner_handoffs WHERE task_id=$1`, task.ID).Scan(&storedBytes); err != nil {
		t.Fatal(err)
	}
	if storedBytes <= MaxPlannerHandoffBytes {
		t.Fatalf("fixture failed to cross jsonb-only whitespace boundary: %d", storedBytes)
	}
}

func TestPlannerHandoffEvidenceVersionOnlyChangeInvalidatesBackground(t *testing.T) {
	d, task := handoffFixture(t)
	var findingID int64
	if err := d.QueryRow(`INSERT INTO findings(task_id,name,summary,evidence,status) VALUES($1,'saved finding','summary','stored evidence','pending') RETURNING id`, task.ID).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	content := handoffContent(t, d, task.ID)
	lease, _ := beginHandoff(t, d, task, "evidence-read")
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "before-evidence-edit", 0, content, time.Now()); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = bumpEvidenceVersionTx(tx, findingID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	projected, err := d.ReconcilePlannerHandoff(t.Context(), task.ID, content)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Decision != "" || !projected.BackgroundMissing || !projected.Todos[0].NeedsReview {
		t.Fatalf("evidence-only correction retained old background %+v", projected)
	}
	latest, err := d.CurrentPlannerHandoffBasis(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ContractFingerprint != content.Basis.ContractFingerprint || latest.EvidenceFingerprint == content.Basis.EvidenceFingerprint {
		t.Fatal("evidence revision did not affect only the evidence fingerprint")
	}
}

func TestPlannerHandoffAuxiliaryWritersCannotDeadlockCompletion(t *testing.T) {
	d, task := handoffFixture(t)
	off := false
	operations := []struct {
		name string
		run  func(context.Context) error
	}{
		{"register", func(ctx context.Context) error {
			return d.RegisterPlannerHandoffInstance(ctx, task.ID, task.ExplorationID, "new-instance")
		}},
		{"invalidate", func(ctx context.Context) error { return d.InvalidatePlannerHandoff(ctx, task.ID) }},
		{"invalidate-all", func(ctx context.Context) error { return d.InvalidateAllPlannerHandoffInstances(ctx) }},
		{"settings", func(ctx context.Context) error { return d.SetPlannerHandoffSettings(ctx, &off, &off) }},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			// Emulate completionTransaction's precise first lock. A direct
			// UPDATE handoff writer would hold tasks RowExclusive while it
			// waits for this row, preventing the subsequent table lock.
			core, err := d.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer core.Rollback()
			var locked int64
			if err = core.QueryRowContext(t.Context(), `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, task.ID).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			var beforeEpoch int64
			var beforeInstance string
			if err = core.QueryRowContext(t.Context(), `SELECT planner_handoff_epoch,planner_handoff_instance_id FROM tasks WHERE id=$1`, task.ID).Scan(&beforeEpoch, &beforeInstance); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				result <- op.run(ctx)
			}()
			select {
			case err = <-result:
				if !errors.Is(err, ErrPlannerHandoffBusy) {
					t.Fatalf("auxiliary operation must fail without waiting for core row: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("auxiliary writer held a relation lock while waiting for completion row")
			}
			// The core operation remains the successful owner and can advance
			// to its existing task-table lock, without being deadlock victim.
			if _, err = core.ExecContext(t.Context(), `SET LOCAL lock_timeout='500ms'; LOCK TABLE tasks IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				t.Fatalf("completion table lock blocked by auxiliary writer: %v", err)
			}
			var afterEpoch int64
			var afterInstance string
			if err = core.QueryRowContext(t.Context(), `SELECT planner_handoff_epoch,planner_handoff_instance_id FROM tasks WHERE id=$1`, task.ID).Scan(&afterEpoch, &afterInstance); err != nil {
				t.Fatal(err)
			}
			if afterEpoch != beforeEpoch || afterInstance != beforeInstance {
				t.Fatal("failed publication/settings mutation leaked task changes")
			}
			if err = core.Commit(); err != nil {
				t.Fatal(err)
			}
			opts, err := d.ReadPlannerHandoffOptions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !opts.Save || !opts.Restore {
				t.Fatal("failed setting change was not atomic")
			}
			// A fresh caller can retry the bounded auxiliary action once the
			// core transaction is out of the critical section.
			if err = op.run(t.Context()); err != nil {
				t.Fatalf("uncontended auxiliary retry failed: %v", err)
			}
			on := true
			if err = d.SetPlannerHandoffSettings(t.Context(), &on, &on); err != nil {
				t.Fatal(err)
			}
			if err = d.RegisterPlannerHandoffInstance(t.Context(), task.ID, task.ExplorationID, "instance-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
