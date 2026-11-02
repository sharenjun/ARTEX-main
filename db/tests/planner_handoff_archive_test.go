//go:build ignore

package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func plannerHandoffArchiveFixture(t *testing.T) *TaskArchiveSnapshot {
	t.Helper()
	content := PlannerHandoffContent{
		FormatVersion: PlannerHandoffFormatVersion,
		Todos:         []PlannerHandoffTodo{{ID: "todo-1", Content: "Verify the remaining check", Status: "pending"}},
		Decision:      "Wait for the existing evidence check.",
		Basis:         PlannerHandoffBasis{ContractFingerprint: "fixture-contract"},
	}
	hash, err := PlannerHandoffContentHash(content)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 11, 2, 1, 2, 3, 0, time.UTC).Format(time.RFC3339Nano)
	current := map[string]any{
		"task_id": int64(42), "exploration_id": int64(73), "current_version": int64(1),
		"last_request_id": "request-1", "instance_id": "instance-1", "run_id": "run-1",
		"epoch": int64(2), "contract_generation": int64(0), "content": content, "content_hash": hash,
		"generated_at": timestamp, "committed_at": timestamp, "checked_at": timestamp,
	}
	history := map[string]any{}
	for key, value := range current {
		history[key] = value
	}
	delete(history, "exploration_id")
	delete(history, "current_version")
	delete(history, "last_request_id")
	history["version"] = int64(1)
	history["request_id"] = "request-1"
	currentRaw, err := json.Marshal([]map[string]any{current})
	if err != nil {
		t.Fatal(err)
	}
	historyRaw, err := json.Marshal([]map[string]any{history})
	if err != nil {
		t.Fatal(err)
	}
	return &TaskArchiveSnapshot{
		FormatVersion: TaskArchiveFormatVersion, TaskID: 42, ExplorationID: 73,
		Tables:     map[string]json.RawMessage{"planner_handoffs": currentRaw, "planner_handoff_history": historyRaw},
		DataCounts: map[string]int64{"planner_handoffs": 1, "planner_handoff_history": 1},
	}
}

func TestValidateArchivePlannerHandoffs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*testing.T, *TaskArchiveSnapshot)
		valid  bool
	}{
		{name: "matching committed version", valid: true},
		{name: "wrong task", change: func(t *testing.T, s *TaskArchiveSnapshot) { s.TaskID++ }},
		{name: "wrong exploration", change: func(t *testing.T, s *TaskArchiveSnapshot) { s.ExplorationID++ }},
		{name: "legacy format cannot carry handoff", change: func(t *testing.T, s *TaskArchiveSnapshot) { s.FormatVersion = 3 }},
		{name: "current pointer without history", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			s.Tables["planner_handoff_history"] = json.RawMessage("[]")
			s.DataCounts["planner_handoff_history"] = 0
		}},
		{name: "history without current pointer", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			s.Tables["planner_handoffs"] = json.RawMessage("[]")
			s.DataCounts["planner_handoffs"] = 0
		}},
		{name: "missing count", change: func(t *testing.T, s *TaskArchiveSnapshot) { delete(s.DataCounts, "planner_handoff_history") }},
		{name: "count mismatch", change: func(t *testing.T, s *TaskArchiveSnapshot) { s.DataCounts["planner_handoff_history"] = 2 }},
		{name: "malformed history", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			s.Tables["planner_handoff_history"] = json.RawMessage("[broken")
		}},
		{name: "malformed content", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "content", map[string]any{"format_version": 2})
		}},
		{name: "content checksum mismatch", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "content_hash", "wrong")
		}},
		{name: "current request does not match receipt", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "last_request_id", "other")
		}},
		{name: "current generation differs from history", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "epoch", int64(5))
		}},
		{name: "invalid timestamp", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "generated_at", "yesterday")
		}},
		{name: "different generation time", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			replaceArchiveHandoffField(t, s, "planner_handoffs", "generated_at", "2026-11-02T02:03:04Z")
		}},
		{name: "oversized package field", change: func(t *testing.T, s *TaskArchiveSnapshot) {
			s.Tables["planner_handoffs"] = json.RawMessage(bytes.Repeat([]byte(" "), 2*MaxPlannerHandoffBytes+4097))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := plannerHandoffArchiveFixture(t)
			if tt.change != nil {
				tt.change(t, snapshot)
			}
			err := ValidateArchivePlannerHandoffs(snapshot)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
}

func TestPlannerHandoffArchiveHistoryBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		versions []int
		valid    bool
	}{
		{name: "retained twenty versions", valid: true},
		{name: "history exceeds retention limit", versions: make([]int, PlannerHandoffHistoryLimit+1)},
		{name: "missing intermediate version", versions: []int{1, 3}},
		{name: "duplicate version", versions: []int{1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := plannerHandoffArchiveFixture(t)
			original, err := decodeArchiveRows(snapshot.Tables["planner_handoff_history"])
			if err != nil {
				t.Fatal(err)
			}
			versions := tt.versions
			if versions == nil {
				versions = make([]int, PlannerHandoffHistoryLimit)
			}
			history := make([]map[string]any, len(versions))
			for i, version := range versions {
				if version == 0 {
					version = i + 1
				}
				row := map[string]any{}
				for key, value := range original[0] {
					row[key] = value
				}
				row["version"] = version
				row["request_id"] = fmt.Sprintf("request-%d", version)
				history[i] = row
			}
			snapshot.Tables["planner_handoff_history"], err = json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.DataCounts["planner_handoff_history"] = int64(len(history))
			latest := history[len(history)-1]
			replaceArchiveHandoffField(t, snapshot, "planner_handoffs", "current_version", latest["version"])
			replaceArchiveHandoffField(t, snapshot, "planner_handoffs", "last_request_id", latest["request_id"])
			err = ValidateArchivePlannerHandoffs(snapshot)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
}

func replaceArchiveHandoffField(t *testing.T, snapshot *TaskArchiveSnapshot, table, field string, value any) {
	t.Helper()
	rows, err := decodeArchiveRows(snapshot.Tables[table])
	if err != nil {
		t.Fatal(err)
	}
	rows[0][field] = value
	snapshot.Tables[table], err = json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPlannerHandoffArchiveLegacyEmpty(t *testing.T) {
	t.Parallel()
	for version := TaskArchiveLegacyFormatVersion; version <= TaskArchiveFormatVersion; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			snapshot := &TaskArchiveSnapshot{FormatVersion: version, TaskID: 42, ExplorationID: 73}
			if err := ValidateArchivePlannerHandoffs(snapshot); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func openPlannerHandoffArchiveDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN must point to an isolated disposable PostgreSQL database")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPlannerHandoffArchiveRoundTrip(t *testing.T) {
	d := openPlannerHandoffArchiveDB(t)
	original, err := d.ReadPlannerHandoffOptions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := d.SetPlannerHandoffSettings(ctx, &original.Save, &original.Restore); err != nil {
			t.Errorf("restore handoff settings: %v", err)
		}
	})
	enabled := true
	if err := d.SetPlannerHandoffSettings(t.Context(), &enabled, &enabled); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("handoff archive", "remaining inspection", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	nodeID, err := d.Exploration(task.ExplorationID).AddNode(KindFact, map[string]any{"summary": "persisted evidence"}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterPlannerHandoffInstance(t.Context(), task.ID, task.ExplorationID, "archive-instance"); err != nil {
		t.Fatal(err)
	}
	lease, _, err := d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "archive-instance", "archive-run")
	if err != nil {
		t.Fatal(err)
	}
	content := PlannerHandoffContent{
		FormatVersion: PlannerHandoffFormatVersion,
		Todos:         []PlannerHandoffTodo{{ID: "remaining-check", Content: "Verify a remaining dependency", Status: "pending", References: []PlannerHandoffReference{{Kind: "node", ID: nodeID, Available: true}}}},
		Decision:      "Keep existing evidence and verify the dependency.",
		Basis:         PlannerHandoffBasis{ContractFingerprint: lease.ContractFingerprint},
	}
	first, err := d.CommitPlannerHandoff(t.Context(), *lease, "archive-request-1", 0, content, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	content.Todos[0].Content = "Verify the dependency after its owner settles"
	saved, err := d.CommitPlannerHandoff(t.Context(), *lease, "archive-request-2", first.Version, content, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FormatVersion != 4 || snapshot.DataCounts["planner_handoffs"] != 1 || snapshot.DataCounts["planner_handoff_history"] != 2 {
		t.Fatalf("snapshot omitted handoff: version=%d counts=%v", snapshot.FormatVersion, snapshot.DataCounts)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/unused/handoff-archive.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"planner_handoffs", "planner_handoff_history"} {
		var count int
		if err := d.QueryRow(`SELECT count(*) FROM `+table+` WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("hot archive retained %s rows=%d", table, count)
		}
	}
	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := d.GetPlannerHandoff(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored == nil || restored.Version != saved.Version || restored.ContentHash != saved.ContentHash || !restored.GeneratedAt.Equal(saved.GeneratedAt) || !restored.CommittedAt.Equal(saved.CommittedAt) || !restored.CheckedAt.Equal(saved.CheckedAt) {
		t.Fatalf("committed handoff changed: restored=%+v saved=%+v", restored, saved)
	}
	if restored.Content.Todos[0].ID != "remaining-check" || restored.Content.Todos[0].References[0].ID != nodeID {
		t.Fatal("todo identity or node reference was remapped")
	}
	history, err := d.PlannerHandoffHistory(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Version != saved.Version {
		t.Fatalf("history=%+v", history)
	}
	var instance string
	var epoch, generation int64
	if err := d.QueryRow(`SELECT planner_handoff_instance_id,planner_handoff_epoch,planner_contract_generation FROM tasks WHERE id=$1`, task.ID).Scan(&instance, &epoch, &generation); err != nil {
		t.Fatal(err)
	}
	if instance != "" || epoch <= lease.Epoch || generation <= lease.ContractGeneration {
		t.Fatalf("old publishing eligibility restored: instance=%q epoch=%d generation=%d", instance, epoch, generation)
	}
	if err := d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	content.Todos[0].Content = "stale archived instance overwrite"
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "late-archive-request", saved.Version, content, time.Now().UTC()); !errors.Is(err, ErrPlannerHandoffStale) {
		t.Fatalf("old instance commit=%v", err)
	}
	// Deletion removes task-owned handoff rows and cannot be undone by a late
	// callback, even after the current version and its history were restored.
	if err := d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"planner_handoffs", "planner_handoff_history"} {
		var count int
		if err := d.QueryRow(`SELECT count(*) FROM `+table+` WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("deleted task retained %s rows=%d", table, count)
		}
	}
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "late-deleted-request", saved.Version, content, time.Now().UTC()); !errors.Is(err, ErrPlannerHandoffParentGone) {
		t.Fatalf("deleted parent commit=%v", err)
	}
}

func TestPlannerHandoffArchiveLegacyDatabaseRestore(t *testing.T) {
	d := openPlannerHandoffArchiveDB(t)
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			task, err := d.CreateTaskWithOptions("old archive without handoff", "legacy", TaskCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
				_ = d.DeleteTask(task.ID)
			})
			if err := d.SetPaused(task.ID, true); err != nil {
				t.Fatal(err)
			}
			archive, err := d.QueueTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := d.SnapshotTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteTaskArchive(archive.ID, snapshot, "/unused/legacy-handoff.tar.zst", "fixture", 1, 1); err != nil {
				t.Fatal(err)
			}
			snapshot.FormatVersion = version
			for _, name := range []string{"planner_handoffs", "planner_handoff_history"} {
				delete(snapshot.Tables, name)
				delete(snapshot.DataCounts, name)
			}
			if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			got, err := d.GetPlannerHandoff(t.Context(), task.ID)
			if err != nil || got != nil {
				t.Fatalf("old package must restore with no handoff: %+v %v", got, err)
			}
		})
	}
}

func TestPlannerHandoffArchiveCanonicalSizeBoundary(t *testing.T) {
	d := openPlannerHandoffArchiveDB(t)
	original, err := d.ReadPlannerHandoffOptions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := d.SetPlannerHandoffSettings(ctx, &original.Save, &original.Restore); err != nil {
			t.Error(err)
		}
	})
	enabled := true
	if err := d.SetPlannerHandoffSettings(t.Context(), &enabled, &enabled); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("archive canonical boundary", "stable contract", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	if err := d.RegisterPlannerHandoffInstance(t.Context(), task.ID, task.ExplorationID, "archive-size-instance"); err != nil {
		t.Fatal(err)
	}
	lease, _, err := d.BeginPlannerHandoff(t.Context(), task.ID, task.ExplorationID, "archive-size-instance", "archive-size-round")
	if err != nil {
		t.Fatal(err)
	}
	basis, err := d.CurrentPlannerHandoffBasis(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	content := PlannerHandoffContent{FormatVersion: PlannerHandoffFormatVersion, Basis: basis, BackgroundMissing: true}
	for i := 0; i < MaxPlannerHandoffTodos; i++ {
		item := PlannerHandoffTodo{ID: fmt.Sprintf("stable-size-%d", i), Content: strings.Repeat("x", 300), Status: "pending"}
		for j := 0; j < 4; j++ {
			item.References = append(item.References, PlannerHandoffReference{Kind: "node", ID: task.ExplorationID, Version: 1, Fingerprint: strings.Repeat("a", 64), Reason: "pending", Available: false})
		}
		content.Todos = append(content.Todos, item)
	}
	// Fill up to the canonical limit with complete todo text. PostgreSQL JSONB
	// adds structural whitespace; that representation must not count as new
	// content or make the archive of a successful commit unavailable.
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	remaining := MaxPlannerHandoffBytes - len(raw) - 16
	if remaining < 0 {
		t.Fatalf("size fixture base exceeded boundary: %d", len(raw))
	}
	for i := range content.Todos {
		extra := min(remaining, 2048-len(content.Todos[i].Content))
		content.Todos[i].Content += strings.Repeat("x", extra)
		remaining -= extra
		if remaining == 0 {
			break
		}
	}
	raw, err = json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != MaxPlannerHandoffBytes-16 {
		t.Fatalf("canonical fixture size %d", len(raw))
	}
	if _, err := d.CommitPlannerHandoff(t.Context(), *lease, "archive-size-request", 0, content, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tables["planner_handoffs"]) <= MaxPlannerHandoffBytes {
		t.Fatal("fixture did not exercise expanded JSONB archive representation")
	}
	if err := ValidateArchivePlannerHandoffs(snapshot); err != nil {
		t.Fatal(err)
	}
	// Package size headroom cannot bypass the original canonical payload limit.
	content.Decision = strings.Repeat("x", 32)
	rows, err := decodeArchiveRows(snapshot.Tables["planner_handoffs"])
	if err != nil {
		t.Fatal(err)
	}
	rows[0]["content"] = content
	snapshot.Tables["planner_handoffs"], err = json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArchivePlannerHandoffs(snapshot); !errors.Is(err, ErrPlannerHandoffTooLarge) {
		t.Fatalf("oversized canonical content accepted: %v", err)
	}
}
