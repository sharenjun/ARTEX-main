package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrGoalProofChanged = errors.New("目标、证据或任务状态已变化，请重新审核")

// GoalStateCounts counts the full local goal set, independent of display pages.
func (s *ExplorationStore) GoalStateCounts(ctx context.Context) (GoalCounts, error) {
	var counts GoalCounts
	err := s.db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE state='met') FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, s.expID).Scan(&counts.Total, &counts.Met)
	return counts, err
}

type GoalProofSnapshot struct {
	TaskID          int64           `json:"task_id"`
	ExplorationID   int64           `json:"exploration_id"`
	TaskGoal        string          `json:"task_goal"`
	TaskDescription string          `json:"task_description"`
	TaskStatus      string          `json:"task_status"`
	Paused          bool            `json:"paused"`
	Queued          bool            `json:"queued"`
	Goal            *Node           `json:"goal"`
	Evidence        *Node           `json:"evidence"`
	Finding         json.RawMessage `json:"finding,omitempty"`
	SupportingFacts json.RawMessage `json:"supporting_facts"`
	Constraints     json.RawMessage `json:"constraints"`
	Scopes          json.RawMessage `json:"scopes"`
}

type GoalAudit struct {
	At             time.Time       `json:"at"`
	Event          string          `json:"event"`
	Actor          string          `json:"actor"`
	Reason         string          `json:"reason"`
	PreviousState  string          `json:"previous_state"`
	GoalVersion    int             `json:"goal_version"`
	EvidenceID     int64           `json:"evidence_id,omitempty"`
	EvidenceDigest string          `json:"evidence_digest,omitempty"`
	PreviousProofs []Edge          `json:"previous_proofs,omitempty"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
}

func goalProofDigest(snapshot GoalProofSnapshot) (string, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func goalEvidenceDigest(snapshot GoalProofSnapshot) (string, error) {
	if len(snapshot.SupportingFacts) == 0 {
		snapshot.SupportingFacts = json.RawMessage(`[]`)
	}
	raw, err := json.Marshal(struct {
		Goal            json.RawMessage
		Evidence        *Node
		Finding         json.RawMessage
		SupportingFacts json.RawMessage
		Constraints     json.RawMessage
		Scopes          json.RawMessage
		TaskGoal        string
		TaskDescription string
	}{snapshot.Goal.Payload, snapshot.Evidence, snapshot.Finding, snapshot.SupportingFacts, snapshot.Constraints, snapshot.Scopes, snapshot.TaskGoal, snapshot.TaskDescription})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Audited met goals must still have a proof for the current saved input. A new
// proof for another finding does not invalidate an unchanged proof. Legacy met
// goals without an audit retain the existing final-review path.
func (s *ExplorationStore) currentGoalProof(ctx context.Context, tx *sql.Tx, goalID int64) (bool, error) {
	var raw json.RawMessage
	if err := tx.QueryRowContext(ctx, `SELECT goal_audit FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, goalID, s.expID).Scan(&raw); err != nil {
		return false, err
	}
	var audits []GoalAudit
	if err := json.Unmarshal(raw, &audits); err != nil {
		return false, err
	}
	audited := false
	for i := len(audits) - 1; i >= 0; i-- {
		audit := audits[i]
		if audit.Event != "prove" {
			continue
		}
		audited = true
		var linked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='proves')`, s.expID, audit.EvidenceID, goalID).Scan(&linked); err != nil {
			return false, err
		}
		if !linked {
			continue
		}
		var saved GoalProofSnapshot
		if json.Unmarshal(audit.Snapshot, &saved) != nil || saved.Goal == nil || saved.Evidence == nil {
			continue
		}
		current, err := s.goalProofSnapshot(ctx, tx, goalID, audit.EvidenceID)
		if errors.Is(err, ErrGoalProofChanged) {
			continue
		}
		if err != nil {
			return false, err
		}
		if ValidateGoalEvidence(current) != nil {
			continue
		}
		expected, err := goalEvidenceDigest(saved)
		if err != nil {
			return false, err
		}
		actual, err := goalEvidenceDigest(current)
		if err != nil {
			return false, err
		}
		if expected == actual {
			return true, nil
		}
	}
	return !audited, nil
}

func (s *ExplorationStore) goalTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	var taskID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL`, s.expID).Scan(&taskID)
	if err == nil {
		return s.db.completionTransaction(ctx, taskID, fn)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Maintenance-only explorations have no task row. They still serialize graph
	// edits; model-driven proofs require a live task and cannot use this fallback.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='5s'; LOCK TABLE exploration_nodes,exploration_edges IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ExplorationStore) goalProofSnapshot(ctx context.Context, tx *sql.Tx, goalID, evidenceID int64) (GoalProofSnapshot, error) {
	out := GoalProofSnapshot{ExplorationID: s.expID}
	if err := tx.QueryRowContext(ctx, `SELECT id,goal,description,status,paused,queued FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL`, s.expID).Scan(&out.TaskID, &out.TaskGoal, &out.TaskDescription, &out.TaskStatus, &out.Paused, &out.Queued); err != nil {
		return out, errors.Join(ErrGoalProofChanged, err)
	}
	if IsTerminal(out.TaskStatus) || out.Paused || out.Queued {
		return out, ErrGoalProofChanged
	}
	var err error
	out.Goal, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal' AND state IN ('open','met')`, goalID, s.expID))
	if err != nil {
		return out, errors.Join(ErrGoalProofChanged, err)
	}
	out.Goal.GoalAudit = nil // Audit history is never part of the model's authority.
	out.Evidence, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND kind IN ('fact','finding') AND state='confirmed' AND (exploration_id=$2 OR exploration_id IN (
	 SELECT source.exploration_id FROM tasks owner JOIN task_relations r ON r.task_id=owner.id JOIN tasks source ON source.id=r.source_task_id AND source.deleted_at IS NULL WHERE owner.id=$3 AND owner.deleted_at IS NULL))`, evidenceID, s.expID, out.TaskID))
	if err != nil {
		return out, errors.Join(ErrGoalProofChanged, err)
	}
	out.Evidence.GoalAudit = nil
	if err = tx.QueryRowContext(ctx, `SELECT `+supportingFactsSQL+` FROM exploration_nodes ev JOIN tasks owner ON owner.id=$2 WHERE ev.id=$1`, evidenceID, out.TaskID).Scan(&out.SupportingFacts); err != nil {
		return out, err
	}
	var sourceTaskID int64
	err = tx.QueryRowContext(ctx, `SELECT source.id FROM exploration_nodes n JOIN tasks source ON source.exploration_id=n.exploration_id AND source.deleted_at IS NULL WHERE n.id=$1 AND n.exploration_id<>$2`, evidenceID, s.expID).Scan(&sourceTaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if sourceTaskID > 0 {
		out.Evidence.SourceTaskID = sourceTaskID
		out.Evidence.Inherited = true
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('id',f.id,'status',f.status,'summary',f.summary,'evidence',f.evidence,'vulnclass',f.vulnclass,'evidence_version',f.evidence_version,'traffic',COALESCE((SELECT jsonb_agg(jsonb_build_object('role',b.role,'note',b.note,'snapshot',to_jsonb(ts)) ORDER BY b.position,b.id) FROM finding_traffic_bindings b JOIN traffic_evidence_snapshots ts ON ts.id=b.snapshot_id WHERE b.finding_id=f.id),'[]'::jsonb)) ORDER BY f.id),'[]'::jsonb) FROM findings f WHERE f.node_id=$1`, evidenceID).Scan(&out.Finding); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM task_constraints c WHERE exploration_id=$1`, s.expID).Scan(&out.Constraints); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(sc) ORDER BY sc.id),'[]'::jsonb) FROM task_scope sc WHERE task_id=$1`, out.TaskID).Scan(&out.Scopes); err != nil {
		return out, err
	}
	return out, nil
}

// Snapshot locks only while reading. The caller performs model review after
// this transaction returns, then commits against the same saved input.
func (s *ExplorationStore) GoalProofSnapshot(ctx context.Context, goalID, evidenceID int64) (GoalProofSnapshot, error) {
	var out GoalProofSnapshot
	err := s.goalTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.goalProofSnapshot(ctx, tx, goalID, evidenceID)
		return err
	})
	return out, err
}

func appendGoalAudit(ctx context.Context, tx *sql.Tx, goalID int64, audit GoalAudit) error {
	raw, err := json.Marshal([]GoalAudit{audit})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET goal_audit=goal_audit||$2::jsonb WHERE id=$1`, goalID, string(raw))
	return err
}

// CommitGoalProof atomically saves the verdict, proof edge and met state. A
// rejected attempt records its gap without claiming that any outcome occurred.
func (s *ExplorationStore) CommitGoalProof(ctx context.Context, snapshot GoalProofSnapshot, supported bool, actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || snapshot.Goal == nil || snapshot.Evidence == nil || snapshot.ExplorationID != s.expID {
		return fmt.Errorf("目标审核缺少来源、理由或快照")
	}
	if supported {
		if err := ValidateGoalEvidence(snapshot); err != nil {
			return err
		}
	}
	expected, err := goalProofDigest(snapshot)
	if err != nil {
		return err
	}
	return s.goalTransaction(ctx, func(tx *sql.Tx) error {
		current, err := s.goalProofSnapshot(ctx, tx, snapshot.Goal.ID, snapshot.Evidence.ID)
		if err != nil {
			return err
		}
		actual, err := goalProofDigest(current)
		if err != nil {
			return err
		}
		if expected != actual {
			return ErrGoalProofChanged
		}
		evidenceDigest, err := goalEvidenceDigest(current)
		if err != nil {
			return err
		}
		if supported && current.Goal.State == "met" {
			var previous bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel='proves' AND dst_id=$3) AND EXISTS(SELECT 1 FROM exploration_nodes n,jsonb_array_elements(n.goal_audit) a WHERE n.id=$3 AND a->>'event'='prove' AND a->>'evidence_digest'=$4)`, s.expID, current.Evidence.ID, current.Goal.ID, evidenceDigest).Scan(&previous); err != nil {
				return err
			}
			if previous {
				return nil
			}
		}
		raw, err := json.Marshal(current)
		if err != nil {
			return err
		}
		event := "reject"
		if supported {
			event = "prove"
		}
		audit := GoalAudit{At: time.Now().UTC(), Event: event, Actor: actor, Reason: reason, PreviousState: current.Goal.State, GoalVersion: current.Goal.ContentVersion, EvidenceID: current.Evidence.ID, EvidenceDigest: evidenceDigest, Snapshot: raw}
		if supported {
			if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,'proves',$3) ON CONFLICT DO NOTHING`, s.expID, current.Evidence.ID, current.Goal.ID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='met',content_version=content_version+1 WHERE id=$1 AND exploration_id=$2`, current.Goal.ID, s.expID); err != nil {
				return err
			}
		}
		return appendGoalAudit(ctx, tx, current.Goal.ID, audit)
	})
}

func goalPreviousProofs(ctx context.Context, tx *sql.Tx, expID, goalID int64) ([]Edge, error) {
	rows, err := tx.QueryContext(ctx, `SELECT src_id,rel,dst_id FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='proves' ORDER BY src_id`, expID, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err = rows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) ReopenGoal(ctx context.Context, goalID int64, version int, actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("目标纠错必须有操作者和原因")
	}
	return s.goalTransaction(ctx, func(tx *sql.Tx) error {
		n, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal' AND state='met' AND content_version=$3`, goalID, s.expID, version))
		if err != nil {
			return errors.Join(ErrGoalProofChanged, err)
		}
		proofs, err := goalPreviousProofs(ctx, tx, s.expID, goalID)
		if err != nil {
			return err
		}
		if err = appendGoalAudit(ctx, tx, goalID, GoalAudit{At: time.Now().UTC(), Event: "reopen", Actor: actor, Reason: reason, PreviousState: n.State, GoalVersion: n.ContentVersion, PreviousProofs: proofs, Snapshot: n.Payload}); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='proves'`, s.expID, goalID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',content_version=content_version+1 WHERE id=$1 AND exploration_id=$2`, goalID, s.expID)
		return err
	})
}

func (s *ExplorationStore) editGoalPayload(id int64, text, vulnclass string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return s.goalTransaction(ctx, func(tx *sql.Tx) error {
		n, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal'`, id, s.expID))
		if err != nil {
			return err
		}
		payload := map[string]any{}
		if err := json.Unmarshal(n.Payload, &payload); err != nil {
			return err
		}
		payload["text"] = text
		delete(payload, "vulnclass")
		if vulnclass != "" {
			payload["vulnclass"] = vulnclass
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var old map[string]any
		if err = json.Unmarshal(n.Payload, &old); err != nil {
			return err
		}
		prior, _ := json.Marshal(old)
		if string(prior) == string(raw) {
			return nil
		}
		proofs, err := goalPreviousProofs(ctx, tx, s.expID, id)
		if err != nil {
			return err
		}
		if err = appendGoalAudit(ctx, tx, id, GoalAudit{At: time.Now().UTC(), Event: "edit", Actor: "human", Reason: "目标内容已修改，撤销旧完成判定并重新审核", PreviousState: n.State, GoalVersion: n.ContentVersion, PreviousProofs: proofs, Snapshot: n.Payload}); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='proves'`, s.expID, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET payload=$1,state='open',content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, string(raw), id, s.expID); err != nil {
			return err
		}
		taskID, err := lockPlannerContractOwner(ctx, tx, s.expID)
		if err != nil {
			return err
		}
		if taskID > 0 {
			return bumpPlannerContract(ctx, tx, taskID)
		}
		return nil
	})
}
