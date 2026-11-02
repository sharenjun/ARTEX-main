package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// resumeWorkerConversation classifies missing history separately from damaged
// history. A pending tool may have produced an external effect; dropping its
// tool_use and starting fresh cannot establish that it never ran.
func resumeWorkerConversation(session *agentcore.Session, store *transcript.Store, sessionID string) (string, error) {
	records, err := store.Load(store.MainPath(sessionID))
	if err != nil {
		return "degraded", &llm.HistoryError{Reason: "worker transcript cannot be read or is corrupt", Cause: err}
	}
	if len(records) == 0 {
		return "fresh", nil
	}
	if toolID := pendingTranscriptTool(records); toolID != "" {
		return "unknown", &llm.HistoryError{Reason: "worker tool execution result is unknown; inspect persisted results before retrying", ToolID: toolID}
	}
	if err = session.Resume(sessionID); err != nil {
		return "degraded", &llm.HistoryError{Reason: "worker conversation resume failed", Cause: err}
	}
	if len(session.Messages()) == 0 {
		return "degraded", &llm.HistoryError{Reason: "worker transcript contains no usable conversation"}
	}
	return "restored", nil
}

// Associations are consumed in record order. Reused IDs after an earlier result
// are independent calls, and an error tool_result still resolves that call.
func pendingTranscriptTool(records []transcript.Record) string {
	pending := map[string]int{}
	var order []string
	for _, record := range records {
		if record.Message == nil {
			continue
		}
		for _, block := range record.Message.Content {
			switch block.Type {
			case llm.BlockToolUse:
				if block.ID != "" {
					pending[block.ID]++
					order = append(order, block.ID)
				}
			case llm.BlockToolResult:
				if pending[block.ToolUseID] > 0 {
					pending[block.ToolUseID]--
				}
			}
		}
	}
	for _, id := range order {
		if pending[id] > 0 {
			return id
		}
	}
	return ""
}

func emitWorkerRecovery(emit func(db.Activity), state string, err error) {
	if emit == nil {
		return
	}
	labels := map[string]string{"restored": "已恢复已保存会话", "degraded": "恢复降级，需核对", "unknown": "上次动作结果未知，需核对", "write_failed": "会话写盘失败，后续恢复可能不完整"}
	summary := "Worker " + labels[state]
	detail := summary
	if err != nil {
		detail += fmt.Sprintf("：%v", err)
	}
	if state != "restored" {
		detail += "。已保存事实、检查点、工具结果和人工消息保留；动作可能已发生但结果未保存时不能当作尚未执行，不根据 Planner 交接盲目重放。"
	}
	metadata, _ := json.Marshal(map[string]any{"worker_recovery": map[string]string{"state": state, "guidance": "verify_saved_results_before_retry"}})
	emit(db.Activity{Kind: "recovery", Summary: summary, Detail: detail, IsError: err != nil, Metadata: metadata})
}

// A newly queued human turn is recorded before its first Worker execution. It
// is not evidence that a side effect ran, so it must not block that first turn.
func priorWorkerExecution(activities []db.Activity) bool {
	for _, activity := range activities {
		if activity.Kind != "user" && activity.Kind != "recovery" {
			return true
		}
	}
	return false
}
