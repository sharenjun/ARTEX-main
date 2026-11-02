package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// getPlannerHandoff is a management-only, read-only view. Reading a handoff
// never creates an Agent, registers a publisher, restores todos or starts work.
func (s *Server) getPlannerHandoff(w http.ResponseWriter, r *http.Request) {
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, http.StatusBadRequest, "无效任务 ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var explorationID int64
	err = s.m.pg.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL`, taskID).Scan(&explorationID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "无法核对交接所属任务")
		return
	}
	options, err := s.m.pg.ReadPlannerHandoffOptions(ctx)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "无法读取 Planner 交接设置")
		return
	}
	current, err := s.m.pg.GetPlannerHandoff(ctx, taskID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "交接记录不可用或格式损坏，当前任务图仍是状态依据")
		return
	}
	history := []db.PlannerHandoff{}
	if r.URL.Query().Get("history") == "1" {
		history, err = s.m.pg.PlannerHandoffHistory(ctx, taskID)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "无法读取 Planner 交接历史")
			return
		}
	}
	status := agent.PlannerHandoffStatus{State: "unsubmitted", RecoveryState: "none"}
	if !options.Save {
		status.State = "disabled"
	} else if current != nil {
		status.State = "submitted"
	}
	if !options.Restore {
		status.RecoveryState = "disabled"
	}
	if current != nil {
		status.Version = current.Version
		committed := current.CommittedAt
		status.CommittedAt = &committed
	}
	s.taskAgentMu.Lock()
	bundle := s.taskAgents[r.PathValue("id")]
	s.taskAgentMu.Unlock()
	if bundle != nil && bundle.pl != nil {
		status = bundle.pl.HandoffStatus(explorationID)
		if !options.Save {
			status.State = "disabled"
		}
		if !options.Restore {
			status.RecoveryState = "disabled"
		}
		if current != nil {
			status.Version = current.Version
			committed := current.CommittedAt
			status.CommittedAt = &committed
		}
	}
	var projection *db.PlannerHandoffContent
	projectionError := ""
	if current != nil {
		reconciled, reconcileErr := s.m.pg.ReconcilePlannerHandoff(ctx, taskID, current.Content)
		if reconcileErr != nil {
			projectionError = "无法按当前任务核对交接；暂不可用"
		} else {
			projection = &reconciled
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"options": options, "status": status, "snapshot": current,
		"projection": projection, "projection_error": projectionError, "history": history,
		"limits": map[string]any{
			"bytes": db.MaxPlannerHandoffBytes, "todos": db.MaxPlannerHandoffTodos,
			"references": db.MaxPlannerHandoffReferences, "history": db.PlannerHandoffHistoryLimit,
		},
	})
}
