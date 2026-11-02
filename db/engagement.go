package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type EngagementGoal struct {
	Text           string   `json:"text"`
	GoalType       string   `json:"goal_type"`
	VulnClass      string   `json:"vulnclass,omitempty"`
	RequiredChecks []string `json:"required_checks"`
}

func (g EngagementGoal) Validate() error {
	if strings.TrimSpace(g.Text) == "" || (g.GoalType != "inspection" && g.GoalType != "outcome") {
		return fmt.Errorf("每个目标须保存 text 与固定 goal_type")
	}
	if g.GoalType == "inspection" && len(g.RequiredChecks) == 0 {
		return fmt.Errorf("检查目标须保存非空 required_checks")
	}
	for _, check := range g.RequiredChecks {
		if strings.TrimSpace(check) == "" {
			return fmt.Errorf("required_checks 不能包含空检查")
		}
	}
	return nil
}

// EngagementChange is a complete, human-authorized replacement. Optional slices
// use nil to preserve the corresponding existing component.
type EngagementChange struct {
	Goal        string           `json:"goal"`
	Description string           `json:"description"`
	Reason      string           `json:"reason"`
	Goals       []EngagementGoal `json:"goals"`
	Constraints []Constraint     `json:"constraints"`
	Scopes      []ScopeInput     `json:"scopes"`
}

// UpdateEngagement atomically updates execution and review inputs. It requires
// paused, quiescent execution; old goals/evidence remain in the change audit.
func (d *DB) UpdateEngagement(ctx context.Context, taskID int64, change EngagementChange) error {
	if strings.TrimSpace(change.Goal) == "" || strings.TrimSpace(change.Reason) == "" || len(change.Goals) == 0 {
		return fmt.Errorf("goal、reason 和完整 goals 必填")
	}
	for _, g := range change.Goals {
		if err := g.Validate(); err != nil {
			return err
		}
	}
	for _, c := range change.Constraints {
		if c.Kind != "allow" && c.Kind != "deny" || strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("无效 constraints")
		}
	}
	return d.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		var expID int64
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT exploration_id,paused FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&expID, &paused); err != nil {
			return err
		}
		if !paused {
			return fmt.Errorf("先暂停并等待当前执行退出，再修改任务契约")
		}
		var running bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='running')`, expID).Scan(&running); err != nil {
			return err
		}
		if running {
			return fmt.Errorf("当前工作尚未退出，稍后重试")
		}
		var prior json.RawMessage
		if err := tx.QueryRowContext(ctx, `SELECT jsonb_build_object('task',to_jsonb(t),'goals',(SELECT jsonb_agg(to_jsonb(n)) FROM exploration_nodes n WHERE exploration_id=$2 AND kind='goal'),'constraints',(SELECT jsonb_agg(to_jsonb(c)) FROM task_constraints c WHERE exploration_id=$2),'scopes',(SELECT jsonb_agg(to_jsonb(s)) FROM task_scope s WHERE task_id=$1)) FROM tasks t WHERE id=$1`, taskID, expID).Scan(&prior); err != nil {
			return err
		}
		raw, err := json.Marshal(change)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_engagement_audits(task_id,reason,previous,current) VALUES($1,$2,$3,$4)`, taskID, change.Reason, string(prior), string(raw)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET goal=$2,description=$3,planner_contract_generation=planner_contract_generation+1,planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=$1;`, taskID, change.Goal, change.Description); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE explorations SET goal=$2,description=$3 WHERE id=$1`, expID, change.Goal, change.Description); err != nil {
			return err
		}
		// Old work names targets under the previous authorization. Keep its
		// checkpoint for audit, but require fresh planning under the new contract.
		if err = retireContractIntents(ctx, tx, expID, change.Reason); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, expID); err != nil {
			return err
		}
		for _, g := range change.Goals {
			raw, _ := json.Marshal(g)
			if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'goal',$2,0,'open','human')`, expID, string(raw)); err != nil {
				return err
			}
		}
		if change.Constraints != nil {
			if _, err = tx.ExecContext(ctx, `DELETE FROM task_constraints WHERE exploration_id=$1`, expID); err != nil {
				return err
			}
			for _, c := range change.Constraints {
				if _, err = tx.ExecContext(ctx, `INSERT INTO task_constraints(exploration_id,kind,text,origin) VALUES($1,$2,$3,'human')`, expID, c.Kind, c.Text); err != nil {
					return err
				}
			}
		}
		if change.Scopes != nil {
			if _, err = tx.ExecContext(ctx, `DELETE FROM task_scope WHERE task_id=$1`, taskID); err != nil {
				return err
			}
			assets := d.Assets()
			assets.tx = tx
			for _, sc := range change.Scopes {
				if _, err = assets.AddAgentScope(taskID, sc.Kind, sc.Value, change.Reason, "manual"); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM task_progress WHERE task_id=$1`, taskID)
		return err
	})
}

func retireContractIntents(ctx context.Context, tx *sql.Tx, expID int64, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE exploration_nodes
SET state='stopped',content_version=content_version+1,
    payload=payload || jsonb_build_object('retired_contract',true,'retired_reason',$2::text)
WHERE exploration_id=$1 AND kind='intent'
  AND state IN ('open','paused','blocked','exhausted','stopped')
  AND COALESCE(payload->>'retired_contract','false')<>'true'`, expID, reason)
	return err
}

func invalidateScopeProofs(ctx context.Context, tx *sql.Tx, expID int64, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' AND state='met'`, expID)
	if err != nil {
		return err
	}
	var goals []*Node
	for rows.Next() {
		n, e := scanNode(rows)
		if e != nil {
			rows.Close()
			return e
		}
		goals = append(goals, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, g := range goals {
		proofs, err := goalPreviousProofs(ctx, tx, expID, g.ID)
		if err != nil {
			return err
		}
		if err = appendGoalAudit(ctx, tx, g.ID, GoalAudit{At: time.Now().UTC(), Event: "scope_edit", Actor: "human", Reason: reason, PreviousState: g.State, GoalVersion: g.ContentVersion, PreviousProofs: proofs, Snapshot: g.Payload}); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM exploration_edges WHERE exploration_id=$1 AND rel='proves'`, expID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',content_version=content_version+1 WHERE exploration_id=$1 AND kind='goal'`, expID)
	return err
}

func (s *AssetStore) EditManualTaskScope(ctx context.Context, taskID, deleteID int64, kind, value, reason string) (TaskScope, error) {
	var out TaskScope
	err := s.db.completionTransaction(ctx, taskID, func(tx *sql.Tx) error {
		var expID int64
		var paused, running bool
		if err := tx.QueryRowContext(ctx, `SELECT exploration_id,paused FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&expID, &paused); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='running')`, expID).Scan(&running); err != nil {
			return err
		}
		if !paused || running {
			return fmt.Errorf("先暂停并等待当前执行退出，再修改范围")
		}
		copy := *s
		copy.tx = tx
		if deleteID > 0 {
			result, err := tx.ExecContext(ctx, `DELETE FROM task_scope WHERE id=$1 AND task_id=$2`, deleteID, taskID)
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			if n == 0 {
				return sql.ErrNoRows
			}
		} else {
			var err error
			out, err = copy.AddAgentScope(taskID, kind, value, reason, "manual")
			if err != nil {
				return err
			}
		}
		if err := bumpPlannerContract(ctx, tx, taskID); err != nil {
			return err
		}
		// Target references also live in free-text intent payloads. Conservatively
		// retire unfinished work and replan, rather than let a resumed old intent
		// restore a removed target through the worker's AddAutoScope call.
		editReason := "人工修改测试范围：" + reason
		if err := retireContractIntents(ctx, tx, expID, editReason); err != nil {
			return err
		}
		return invalidateScopeProofs(ctx, tx, expID, editReason)
	})
	return out, err
}
