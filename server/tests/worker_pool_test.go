//go:build ignore

package server

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestWorkerPoolProfileIDUsesStableRoundRobinSlots(t *testing.T) {
	ids := []int64{11, 22, 33}
	for name, want := range map[string]int64{
		"work#1": 11,
		"work#2": 22,
		"work#3": 33,
		"work#4": 11,
		"work#7": 11,
	} {
		got, ok := workerPoolProfileID(ids, name)
		if !ok || got != want {
			t.Fatalf("workerPoolProfileID(%q) = (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}
	if _, ok := workerPoolProfileID(nil, "work#1"); ok {
		t.Fatal("empty worker pool must leave the existing resolver in control")
	}
}

func TestValidateWorkerPoolProfileIDs(t *testing.T) {
	if err := validateWorkerPoolProfileIDs([]int64{1, 2, 3}); err != nil {
		t.Fatalf("valid profile list rejected: %v", err)
	}
	for _, ids := range [][]int64{{0}, {-1}, {4, 4}} {
		if err := validateWorkerPoolProfileIDs(ids); err == nil {
			t.Fatalf("invalid profile list %v was accepted", ids)
		}
	}
	tooMany := make([]int64, maxWorkerPoolProfiles+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if err := validateWorkerPoolProfileIDs(tooMany); err == nil {
		t.Fatal("worker pool accepted more than the configured maximum")
	}
}

func TestWorkerPoolOnlyChangesUnboundTasksWithoutTaskChains(t *testing.T) {
	pool := []int64{11, 22}
	task := &Task{}
	if taskUsesWorkerPool(task, nil, false) {
		t.Fatal("empty pool must keep the shared Worker behavior")
	}
	if !taskUsesWorkerPool(task, pool, false) {
		t.Fatal("an unbound task should use the configured pool")
	}
	if taskUsesWorkerPool(task, pool, true) {
		t.Fatal("an explicitly bound Worker should keep its binding")
	}
	profileID := int64(5)
	task.setLLMState(&profileID, &profileID, []int64{profileID}, 1, "ready", "")
	if taskUsesWorkerPool(task, pool, false) {
		t.Fatal("an explicit task failover chain should take precedence")
	}
}

func TestWorkerPoolTaskSnapshotKeepsProfilesAndWorkerCount(t *testing.T) {
	task := &Task{ID: "7"}
	s := &Server{
		taskAgents: map[string]*taskAgentBundle{
			"7": {workerPoolIDs: []int64{11, 22}, workerSlots: 4},
		},
	}
	ids, err := s.workerPoolProfilesForTask(task)
	if err != nil || fmt.Sprint(ids) != "[11 22]" {
		t.Fatalf("task pool snapshot=%v, err=%v", ids, err)
	}
	if got := s.workerSlotsForTask(task); got != 4 {
		t.Fatalf("task worker slot snapshot=%d, want 4", got)
	}
}

func TestWorkerPoolTaskResolutionRespectsPrecedence(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	oldPool, hadPool, err := m.pg.GetSetting(settingWorkerPoolProfiles)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPool {
			if err := m.pg.SetSetting(settingWorkerPoolProfiles, oldPool); err != nil {
				t.Errorf("restore Worker model pool setting: %v", err)
			}
		} else if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingWorkerPoolProfiles); err != nil {
			t.Errorf("remove Worker model pool setting: %v", err)
		}
	})
	previousBinding, err := m.pg.GetAgentByKey("worker")
	if err != nil {
		t.Fatal(err)
	}
	var oldBinding *int64
	if previousBinding != nil && previousBinding.LLMProfileID != nil {
		id := *previousBinding.LLMProfileID
		oldBinding = &id
	}
	stamp := time.Now().UnixNano()
	makeProfile := func(name, model string) int64 {
		t.Helper()
		id, err := m.pg.SaveProfile(&db.LLMProfile{
			Name: fmt.Sprintf("%s-%d", name, stamp), Format: "openai",
			Model: model, BaseURL: "https://api.example.test/v1", APIKey: "test-key",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := m.pg.DeleteProfile(id); err != nil {
				t.Errorf("delete profile #%d: %v", id, err)
			}
		})
		return id
	}
	poolOne := makeProfile("worker-pool-one", "pool-model-one")
	poolTwo := makeProfile("worker-pool-two", "pool-model-two")
	chainProfile := makeProfile("worker-pool-chain", "chain-model")
	boundProfile := makeProfile("worker-pool-bound", "bound-model")
	t.Cleanup(func() {
		if err := m.pg.SetAgentLLMProfile("worker", oldBinding); err != nil {
			t.Errorf("restore Worker Agent binding: %v", err)
		}
	})
	if err := m.pg.SetAgentLLMProfile("worker", nil); err != nil {
		t.Fatal(err)
	}
	poolJSON, err := json.Marshal([]int64{poolOne, poolTwo})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingWorkerPoolProfiles, string(poolJSON)); err != nil {
		t.Fatal(err)
	}

	poolTask, err := m.CreateTaskWithOptions("Worker pool precedence", "assign workers", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(poolTask.ID, DeleteTaskOptions{}) })
	chainTask, err := m.CreateTaskWithOptions("Worker task chain precedence", "assign workers", db.TaskCreateOptions{
		LLMProfileIDs: []int64{chainProfile},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(chainTask.ID, DeleteTaskOptions{}) })

	s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
	assertResolution := func(task *Task, slot, wantSource, wantModel string) {
		t.Helper()
		got, err := s.resolveTaskSlotLLM(task, "worker", slot)
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != wantSource || got.Model != wantModel {
			t.Fatalf("slot %s resolution=%+v, want source=%q model=%q", slot, got, wantSource, wantModel)
		}
	}
	assertResolution(poolTask, "work#1", "worker_pool", "pool-model-one")
	assertResolution(poolTask, "work#2", "worker_pool", "pool-model-two")
	assertResolution(chainTask, "work#1", "task_chain", "chain-model")
	if err := m.pg.SetAgentLLMProfile("worker", &boundProfile); err != nil {
		t.Fatal(err)
	}
	assertResolution(poolTask, "work#1", "agent_binding", "bound-model")
	assertResolution(chainTask, "work#2", "agent_binding", "bound-model")
}
