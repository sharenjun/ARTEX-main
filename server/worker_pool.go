package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

const (
	settingWorkerPoolProfiles = "worker_pool_profile_ids"
	maxWorkerPoolProfiles     = 32
)

func validateWorkerPoolProfileIDs(ids []int64) error {
	if len(ids) > maxWorkerPoolProfiles {
		return fmt.Errorf("Worker 模型池最多可选择 %d 个配置", maxWorkerPoolProfiles)
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return fmt.Errorf("Worker 模型池必须使用不重复的正整数配置 ID")
		}
		seen[id] = true
	}
	return nil
}

func (m *Manager) workerPoolProfileIDs() ([]int64, error) {
	if m == nil || m.pg == nil {
		return nil, nil
	}
	raw, present, err := m.pg.GetSetting(settingWorkerPoolProfiles)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	if !present || strings.TrimSpace(raw) == "" {
		return ids, nil
	}
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("invalid Worker model pool: %w", err)
	}
	return ids, validateWorkerPoolProfileIDs(ids)
}

// WorkerPoolProfileIDs returns the configured order; an empty list preserves
// the existing role binding, task chain and global selection behavior.
func (m *Manager) WorkerPoolProfileIDs() []int64 {
	ids, _ := m.workerPoolProfileIDs()
	if ids == nil {
		return []int64{}
	}
	return ids
}

func (s *Server) validateWorkerPoolProfiles(ids []int64) error {
	if err := validateWorkerPoolProfileIDs(ids); err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, ok := s.providerForProfile(id); !ok {
			return fmt.Errorf("Worker 模型池配置 #%d 不存在或无效", id)
		}
	}
	return nil
}

func workerSlotIndex(name string) int {
	if strings.HasPrefix(name, "work#") {
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "work#")); err == nil && n > 0 {
			return n - 1
		}
	}
	return 0
}

func workerPoolProfileID(ids []int64, name string) (int64, bool) {
	if len(ids) == 0 {
		return 0, false
	}
	return ids[workerSlotIndex(name)%len(ids)], true
}

func taskUsesWorkerPool(t *Task, ids []int64, hasAgentBinding bool) bool {
	return t != nil && !hasAgentBinding && len(ids) > 0 && len(t.llmStateSnapshot().ProfileIDs) == 0
}

// workerPoolProvider deliberately uses the selected profile directly: pool slots
// are parallel assignments, not a failover chain, and must not silently drift to
// another model when a selected profile becomes unavailable.
func (s *Server) workerPoolProvider(name string) (llm.Provider, agent.Config, bool, error) {
	ids, err := s.m.workerPoolProfileIDs()
	if err != nil {
		return nil, agent.Config{}, true, err
	}
	id, selected := workerPoolProfileID(ids, name)
	if !selected {
		return nil, agent.Config{}, false, nil
	}
	prov, cfg, ok := s.providerForProfile(id)
	if !ok {
		return nil, agent.Config{}, true, fmt.Errorf("Worker slot %s profile #%d is missing or invalid", name, id)
	}
	return prov, cfg, true, nil
}

func (s *Server) workerPoolProfilesForTask(t *Task) ([]int64, error) {
	if t == nil {
		return nil, fmt.Errorf("task unavailable")
	}
	s.taskAgentMu.Lock()
	bundle := s.taskAgents[t.ID]
	if bundle != nil {
		ids, err := append([]int64(nil), bundle.workerPoolIDs...), bundle.workerPoolErr
		s.taskAgentMu.Unlock()
		return ids, err
	}
	s.taskAgentMu.Unlock()
	return s.m.workerPoolProfileIDs()
}

func (s *Server) workerSlotsForTask(t *Task) int {
	if t != nil {
		s.taskAgentMu.Lock()
		if bundle := s.taskAgents[t.ID]; bundle != nil {
			slots := bundle.workerSlots
			s.taskAgentMu.Unlock()
			if slots > 0 {
				return slots
			}
			slots = s.m.Workers()
			if slots > 0 {
				return slots
			}
			return defaultWorkers
		}
		s.taskAgentMu.Unlock()
	}
	slots := s.m.Workers()
	if slots > 0 {
		return slots
	}
	return defaultWorkers
}

func (s *Server) newTaskWorker(runtime *taskLLMRuntime, tx *transcript.Store) *agent.Worker {
	wk := agent.NewWorker(runtime, "task-router", s.m.dir, tx, runtime.CompactionWindow(), s.agentMaxTurns("worker"))
	wk.SetFindingRecorder(s.evidenceStore())
	wk.SetCompactionWindowResolver(runtime.CompactionWindow)
	wk.SetNonStreaming(runtime.nonStreaming)
	wk.SetMaxTokens(runtime.maxTokens)
	wk.SetNoaEnabled(s.m.NoaCompactionEnabled)
	wk.SetRunTimeout(time.Duration(s.agentRunSeconds("worker")) * time.Second)
	wk.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	wk.SetWebSearch(s.webSearchFor("worker"))
	wk.SetConstraintInject(s.constraintInjectWorker)
	return wk
}

// workerForTask keeps each slot's provider, compaction and per-run settings on
// the same runtime. The cache is protected by the existing task-agent mutex.
func (s *Server) workerForTask(t *Task, name string) *agent.Worker {
	name = fmt.Sprintf("work#%d", workerSlotIndex(name)+1)
	b := s.agentsForTask(t)
	_, _, bound := s.agentBindingProvider("worker")
	usePool := taskUsesWorkerPool(t, b.workerPoolIDs, bound)
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if !usePool {
		return b.wk
	}
	if b.workers == nil {
		b.workers = map[string]*agent.Worker{"work#1": b.wk}
	}
	if wk := b.workers[name]; wk != nil {
		return wk
	}
	runtime := &taskLLMRuntime{
		s: s, taskID: t.ID, agentKey: "worker", workerSlot: name,
		workerPoolIDs: b.workerPoolIDs, workerPoolErr: b.workerPoolErr, workerPoolPinned: true,
	}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	wk := s.newTaskWorker(runtime, tx)
	b.workers[name] = wk
	return wk
}
