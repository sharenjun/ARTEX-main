package db

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ValidateArchivePlannerHandoffs checks the complete committed handoff projection
// before a package can install files or restore task-owned rows. Versions 1-3 did
// not contain handoffs and continue to restore with an empty handoff.
func ValidateArchivePlannerHandoffs(snapshot *TaskArchiveSnapshot) error {
	if snapshot == nil || !IsTaskArchiveFormatSupported(snapshot.FormatVersion) {
		return ErrTaskArchiveFormatMismatch
	}
	// Bound manifest data before decoding even a malformed package. A database
	// row allows PostgreSQL JSONB whitespace plus 4 KiB of identity/time metadata.
	// Typed canonical validation below still enforces the 64 KiB content limit.
	const rowLimit = 2*MaxPlannerHandoffBytes + (4 << 10)
	if len(snapshot.Tables["planner_handoffs"]) > rowLimit || len(snapshot.Tables["planner_handoff_history"]) > rowLimit*PlannerHandoffHistoryLimit {
		return fmt.Errorf("invalid archived planner handoff size: %w", ErrPlannerHandoffTooLarge)
	}
	currentRows, err := decodeArchiveRows(snapshot.Tables["planner_handoffs"])
	if err != nil {
		return fmt.Errorf("invalid archived planner handoff: %w", err)
	}
	historyRows, err := decodeArchiveRows(snapshot.Tables["planner_handoff_history"])
	if err != nil {
		return fmt.Errorf("invalid archived planner handoff history: %w", err)
	}
	if snapshot.FormatVersion < 4 && (len(currentRows) != 0 || len(historyRows) != 0) {
		return fmt.Errorf("%w: planner handoffs require archive format 4", ErrTaskArchiveFormatMismatch)
	}
	for name, count := range map[string]int64{
		"planner_handoffs":        int64(len(currentRows)),
		"planner_handoff_history": int64(len(historyRows)),
	} {
		expected, ok := snapshot.DataCounts[name]
		if !ok && count > 0 {
			return fmt.Errorf("archived %s row count is missing", name)
		}
		if ok && expected != count {
			return fmt.Errorf("invalid archived %s count: %d does not match %d", name, count, expected)
		}
	}
	if len(currentRows) == 0 && len(historyRows) == 0 {
		return nil
	}
	if len(currentRows) != 1 || len(historyRows) == 0 || len(historyRows) > PlannerHandoffHistoryLimit {
		return fmt.Errorf("invalid archived planner handoff current/history size")
	}
	current := currentRows[0]
	taskID, taskOK := jsonInt64(current["task_id"])
	expID, expOK := jsonInt64(current["exploration_id"])
	version, versionOK := jsonInt64(current["current_version"])
	if !taskOK || !expOK || !versionOK || taskID != snapshot.TaskID || expID != snapshot.ExplorationID || version <= 0 {
		return fmt.Errorf("invalid archived planner handoff identity")
	}
	if err := validateArchivedHandoffContent(current); err != nil {
		return err
	}
	versions := make(map[int64]map[string]any, len(historyRows))
	requests := make(map[string]bool, len(historyRows))
	for _, row := range historyRows {
		owner, ownerOK := jsonInt64(row["task_id"])
		revision, revisionOK := jsonInt64(row["version"])
		request, _ := row["request_id"].(string)
		if !ownerOK || !revisionOK || owner != taskID || revision <= 0 || revision > version || versions[revision] != nil || strings.TrimSpace(request) == "" || !handoffTextOK(request, 128) || requests[request] {
			return fmt.Errorf("invalid archived planner handoff history identity")
		}
		if err := validateArchivedHandoffContent(row); err != nil {
			return err
		}
		versions[revision] = row
		requests[request] = true
	}
	latest := versions[version]
	if latest == nil {
		return fmt.Errorf("archived planner handoff current version has no matching history")
	}
	currentRequest, _ := current["last_request_id"].(string)
	if currentRequest != latest["request_id"] || current["content_hash"] != latest["content_hash"] {
		return fmt.Errorf("archived planner handoff current version does not match history")
	}
	for _, name := range []string{"instance_id", "run_id", "epoch", "contract_generation"} {
		if current[name] != latest[name] {
			return fmt.Errorf("archived planner handoff generating identity does not match history")
		}
	}
	if len(versions) > 0 {
		first := version - int64(len(versions)) + 1
		for revision := first; revision <= version; revision++ {
			if versions[revision] == nil {
				return fmt.Errorf("archived planner handoff history has missing versions")
			}
		}
	}
	for _, name := range []string{"generated_at", "committed_at", "checked_at"} {
		if !sameArchivedHandoffTimestamp(current[name], latest[name]) {
			return fmt.Errorf("archived planner handoff %s does not match history", name)
		}
	}
	return nil
}

func validateArchivedHandoffContent(row map[string]any) error {
	for _, field := range []string{"instance_id", "run_id"} {
		value, ok := row[field].(string)
		if !ok || strings.TrimSpace(value) == "" || !handoffTextOK(value, 128) {
			return fmt.Errorf("invalid archived planner handoff %s", field)
		}
	}
	epoch, epochOK := jsonInt64(row["epoch"])
	generation, generationOK := jsonInt64(row["contract_generation"])
	if !epochOK || epoch <= 0 || !generationOK || generation < 0 {
		return fmt.Errorf("invalid archived planner handoff generation")
	}
	raw, err := json.Marshal(row["content"])
	if err != nil {
		return err
	}
	if len(raw) > MaxPlannerHandoffBytes {
		return ErrPlannerHandoffTooLarge
	}
	var content PlannerHandoffContent
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&content); err != nil {
		return fmt.Errorf("invalid archived planner handoff content: %w", err)
	}
	if err := ValidatePlannerHandoffContent(content); err != nil {
		return fmt.Errorf("invalid archived planner handoff content: %w", err)
	}
	hash, err := PlannerHandoffContentHash(content)
	if err != nil {
		return err
	}
	if row["content_hash"] != hash {
		return fmt.Errorf("invalid archived planner handoff content hash")
	}
	for _, field := range []string{"generated_at", "committed_at", "checked_at"} {
		text, ok := row[field].(string)
		if !ok {
			return fmt.Errorf("invalid archived planner handoff %s", field)
		}
		if value, err := time.Parse(time.RFC3339Nano, text); err != nil || value.IsZero() {
			return fmt.Errorf("invalid archived planner handoff %s", field)
		}
	}
	return nil
}

func sameArchivedHandoffTimestamp(left, right any) bool {
	a, aOK := left.(string)
	b, bOK := right.(string)
	if !aOK || !bOK {
		return false
	}
	aTime, aErr := time.Parse(time.RFC3339Nano, a)
	bTime, bErr := time.Parse(time.RFC3339Nano, b)
	return aErr == nil && bErr == nil && aTime.Equal(bTime)
}

func restorePlannerHandoffsTx(tx *sql.Tx, snapshot *TaskArchiveSnapshot) error {
	current, err := decodeArchiveRows(snapshot.Tables["planner_handoffs"])
	if err != nil || len(current) == 0 {
		return err
	}
	// Node and finding IDs are restored unchanged by the task archive. Handoff
	// references only support those IDs, so asset remapping cannot silently
	// retarget a handoff. Keep the original content, basis, and generation times
	// for recovery reconciliation. The identities here describe historical
	// producers; live publishing eligibility was revoked on the task stub.
	if err := insertArchiveRows(tx, "planner_handoff_history", snapshot.Tables["planner_handoff_history"]); err != nil {
		return err
	}
	return insertArchiveRows(tx, "planner_handoffs", snapshot.Tables["planner_handoffs"])
}
