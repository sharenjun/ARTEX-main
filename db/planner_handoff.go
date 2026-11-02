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
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	PlannerHandoffSaveSetting            = "planner_handoff_save"
	PlannerHandoffRestoreSetting         = "planner_handoff_restore"
	PlannerHandoffFormatVersion          = 1
	MaxPlannerHandoffBytes               = 64 << 10
	MaxPlannerHandoffTodos               = 64
	MaxPlannerHandoffReferences          = 256
	PlannerHandoffHistoryLimit           = 20
	MaxPlannerHandoffCommitsPerRun       = 20
	plannerHandoffGate             int64 = 7337741008
)

var (
	ErrPlannerHandoffDisabled   = errors.New("planner handoff saving is disabled")
	ErrPlannerHandoffStale      = errors.New("planner handoff publisher or task contract changed")
	ErrPlannerHandoffConflict   = errors.New("planner handoff version changed")
	ErrPlannerHandoffParentGone = errors.New("planner handoff task is unavailable")
	ErrPlannerHandoffTooLarge   = errors.New("planner handoff exceeds storage limits")
	ErrPlannerHandoffBusy       = errors.New("planner handoff task is busy")
)

type PlannerHandoffOptions struct {
	Save    bool `json:"save"`
	Restore bool `json:"restore"`
}

type PlannerHandoffReference struct {
	Kind        string `json:"kind"`
	ID          int64  `json:"id"`
	Version     int64  `json:"version,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
}

type PlannerHandoffTodo struct {
	ID          string                    `json:"id"`
	Content     string                    `json:"content"`
	Status      string                    `json:"status"`
	NeedsReview bool                      `json:"needs_review,omitempty"`
	References  []PlannerHandoffReference `json:"references,omitempty"`
}

type PlannerHandoffBasis struct {
	ContractFingerprint string                    `json:"contract_fingerprint"`
	EvidenceFingerprint string                    `json:"evidence_fingerprint"`
	References          []PlannerHandoffReference `json:"references,omitempty"`
}

type PlannerHandoffContent struct {
	FormatVersion         int                  `json:"format_version"`
	Todos                 []PlannerHandoffTodo `json:"todos"`
	Decision              string               `json:"decision,omitempty"`
	DeferredReason        string               `json:"deferred_reason,omitempty"`
	ReconsiderWhen        string               `json:"reconsider_when,omitempty"`
	NextDependencies      string               `json:"next_dependencies,omitempty"`
	BackgroundMissing     bool                 `json:"background_missing"`
	BackgroundGeneratedAt *time.Time           `json:"background_generated_at,omitempty"`
	Basis                 PlannerHandoffBasis  `json:"basis"`
}

type PlannerHandoffLease struct {
	TaskID              int64  `json:"task_id"`
	ExplorationID       int64  `json:"exploration_id"`
	InstanceID          string `json:"instance_id"`
	RunID               string `json:"run_id"`
	Epoch               int64  `json:"epoch"`
	ContractGeneration  int64  `json:"contract_generation"`
	ContractFingerprint string `json:"contract_fingerprint"`
}

type PlannerHandoff struct {
	TaskID             int64                 `json:"task_id"`
	ExplorationID      int64                 `json:"exploration_id"`
	Version            int64                 `json:"version"`
	RequestID          string                `json:"request_id"`
	InstanceID         string                `json:"instance_id"`
	RunID              string                `json:"run_id"`
	Epoch              int64                 `json:"epoch"`
	ContractGeneration int64                 `json:"contract_generation"`
	Content            PlannerHandoffContent `json:"content"`
	ContentHash        string                `json:"content_hash"`
	GeneratedAt        time.Time             `json:"generated_at"`
	CommittedAt        time.Time             `json:"committed_at"`
	CheckedAt          time.Time             `json:"checked_at"`
}

func handoffTextOK(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// ValidatePlannerHandoffContent rejects oversized content as a whole. Critical
// gaps, dependencies and references are never silently truncated.
func ValidatePlannerHandoffContent(c PlannerHandoffContent) error {
	if c.FormatVersion != PlannerHandoffFormatVersion {
		return fmt.Errorf("unsupported planner handoff format %d", c.FormatVersion)
	}
	if len(c.Todos) > MaxPlannerHandoffTodos || len(c.Basis.References) > MaxPlannerHandoffReferences {
		return ErrPlannerHandoffTooLarge
	}
	for _, s := range []string{c.Decision, c.DeferredReason, c.ReconsiderWhen, c.NextDependencies} {
		if !handoffTextOK(s, 4096) {
			return ErrPlannerHandoffTooLarge
		}
	}
	if !handoffTextOK(c.Basis.ContractFingerprint, 128) || !handoffTextOK(c.Basis.EvidenceFingerprint, 128) {
		return ErrPlannerHandoffTooLarge
	}
	ids := map[string]bool{}
	count := len(c.Basis.References)
	checkRef := func(r PlannerHandoffReference) error {
		if r.ID <= 0 || (r.Kind != "node" && r.Kind != "finding") || r.Version < 0 {
			return fmt.Errorf("invalid planner handoff reference")
		}
		if !handoffTextOK(r.Fingerprint, 128) || !handoffTextOK(r.Reason, 512) {
			return ErrPlannerHandoffTooLarge
		}
		return nil
	}
	for _, r := range c.Basis.References {
		if err := checkRef(r); err != nil {
			return err
		}
	}
	for _, item := range c.Todos {
		if strings.TrimSpace(item.ID) == "" || ids[item.ID] || !handoffTextOK(item.ID, 128) || strings.TrimSpace(item.Content) == "" {
			return fmt.Errorf("invalid or duplicate planner todo identity")
		}
		ids[item.ID] = true
		if !handoffTextOK(item.Content, 2048) {
			return ErrPlannerHandoffTooLarge
		}
		if item.Status != "pending" && item.Status != "in_progress" && item.Status != "completed" {
			return fmt.Errorf("invalid planner todo status")
		}
		count += len(item.References)
		if count > MaxPlannerHandoffReferences {
			return ErrPlannerHandoffTooLarge
		}
		for _, r := range item.References {
			if err := checkRef(r); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(raw) > MaxPlannerHandoffBytes {
		return ErrPlannerHandoffTooLarge
	}
	return nil
}

func PlannerHandoffContentHash(c PlannerHandoffContent) (string, error) {
	if err := ValidatePlannerHandoffContent(c); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type handoffQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func plannerHandoffOptions(ctx context.Context, q handoffQuerier) (PlannerHandoffOptions, error) {
	var out PlannerHandoffOptions
	rows, err := q.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ($1,$2)`, PlannerHandoffSaveSetting, PlannerHandoffRestoreSetting)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, err
		}
		if value != "true" && value != "false" && value != "1" && value != "0" {
			return out, fmt.Errorf("invalid planner handoff setting")
		}
		enabled := value == "true" || value == "1"
		if key == PlannerHandoffSaveSetting {
			out.Save = enabled
		} else {
			out.Restore = enabled
		}
	}
	return out, rows.Err()
}

func (d *DB) ReadPlannerHandoffOptions(ctx context.Context) (PlannerHandoffOptions, error) {
	return plannerHandoffOptions(ctx, d)
}

// All handoff operations acquire this gate before task rows. Exclusive setting
// changes atomically revoke every in-flight lease, including off/on transitions.
func handoffGate(ctx context.Context, tx *sql.Tx, exclusive bool) error {
	query := `SELECT pg_advisory_xact_lock_shared($1)`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock($1)`
	}
	_, err := tx.ExecContext(ctx, query, plannerHandoffGate)
	return err
}

// completionTransaction locks an owner row before taking a task-table lock.
// UPDATE would take RowExclusive first and then wait for that row, creating a
// cycle. Prelock every target with compatible RowShare and never wait after
// partially acquiring a multi-task set. Contention only fails this auxiliary
// handoff operation; it cannot choose a core completion transaction as victim.
func prelockPlannerHandoffTasks(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, plannerHandoffLockError(err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		ids = append(ids, taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, plannerHandoffLockError(err)
	}
	return ids, nil
}

func plannerHandoffLockError(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "55P03" {
		return fmt.Errorf("%w: publisher/settings change was not applied", ErrPlannerHandoffBusy)
	}
	return err
}

func (d *DB) SetPlannerHandoffSettings(ctx context.Context, save, restore *bool) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, true); err != nil {
		return err
	}
	current, err := plannerHandoffOptions(ctx, tx)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range []struct {
		key   string
		value *bool
		old   bool
	}{{PlannerHandoffSaveSetting, save, current.Save}, {PlannerHandoffRestoreSetting, restore, current.Restore}} {
		if entry.value == nil {
			continue
		}
		value := "false"
		if *entry.value {
			value = "true"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, entry.key, value); err != nil {
			return err
		}
		changed = changed || *entry.value != entry.old
	}
	if changed {
		ids, lockErr := prelockPlannerHandoffTasks(ctx, tx, `SELECT id FROM tasks WHERE deleted_at IS NULL ORDER BY id FOR UPDATE NOWAIT`)
		if lockErr != nil {
			return lockErr
		}
		// READ COMMITTED may see tasks created after the prelock query. Only
		// mutate the exact owner set already acquired above.
		if len(ids) > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE tasks SET planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=ANY($1::bigint[])`, ids); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Register is a host assembly operation, never a model tool or per-round retry.
func (d *DB) RegisterPlannerHandoffInstance(ctx context.Context, taskID, expID int64, instanceID string) error {
	if instanceID == "" || !handoffTextOK(instanceID, 128) {
		return fmt.Errorf("invalid planner instance identity")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, false); err != nil {
		return err
	}
	ids, err := prelockPlannerHandoffTasks(ctx, tx, `SELECT id FROM tasks WHERE id=$1 AND exploration_id=$2 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE NOWAIT`, taskID, expID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrPlannerHandoffParentGone
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET planner_handoff_instance_id=$3,planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=$1 AND exploration_id=$2 AND deleted_at IS NULL AND archived_at IS NULL`, taskID, expID, instanceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrPlannerHandoffParentGone
	}
	return tx.Commit()
}

func (d *DB) InvalidatePlannerHandoff(ctx context.Context, taskID int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, false); err != nil {
		return err
	}
	ids, err := prelockPlannerHandoffTasks(ctx, tx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE NOWAIT`, taskID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET planner_handoff_instance_id='',planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=ANY($1::bigint[])`, ids); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) InvalidateAllPlannerHandoffInstances(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, true); err != nil {
		return err
	}
	ids, err := prelockPlannerHandoffTasks(ctx, tx, `SELECT id FROM tasks WHERE deleted_at IS NULL ORDER BY id FOR UPDATE NOWAIT`)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET planner_handoff_instance_id='',planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=ANY($1::bigint[])`, ids); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const plannerHandoffColumns = `task_id,exploration_id,current_version,last_request_id,instance_id,run_id,epoch,contract_generation,content,content_hash,generated_at,committed_at,checked_at`

func scanPlannerHandoff(row interface{ Scan(...any) error }) (*PlannerHandoff, error) {
	var out PlannerHandoff
	var raw []byte
	err := row.Scan(&out.TaskID, &out.ExplorationID, &out.Version, &out.RequestID, &out.InstanceID, &out.RunID, &out.Epoch, &out.ContractGeneration, &raw, &out.ContentHash, &out.GeneratedAt, &out.CommittedAt, &out.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxPlannerHandoffBytes*2 {
		return nil, ErrPlannerHandoffTooLarge
	}
	if err = json.Unmarshal(raw, &out.Content); err != nil {
		return nil, fmt.Errorf("decode planner handoff: %w", err)
	}
	hash, err := PlannerHandoffContentHash(out.Content)
	if err != nil {
		return nil, err
	}
	if hash != out.ContentHash {
		return nil, fmt.Errorf("planner handoff content checksum mismatch")
	}
	return &out, nil
}

func (d *DB) GetPlannerHandoff(ctx context.Context, taskID int64) (*PlannerHandoff, error) {
	return scanPlannerHandoff(d.QueryRowContext(ctx, `SELECT `+plannerHandoffColumns+` FROM planner_handoffs h WHERE task_id=$1 AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=h.task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL)`, taskID))
}

func (d *DB) PlannerHandoffHistory(ctx context.Context, taskID int64) ([]PlannerHandoff, error) {
	rows, err := d.QueryContext(ctx, `SELECT h.task_id,t.exploration_id,h.version,h.request_id,h.instance_id,h.run_id,h.epoch,h.contract_generation,h.content,h.content_hash,h.generated_at,h.committed_at,h.checked_at FROM planner_handoff_history h JOIN tasks t ON t.id=h.task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL WHERE h.task_id=$1 ORDER BY h.version DESC LIMIT $2`, taskID, PlannerHandoffHistoryLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlannerHandoff{}
	for rows.Next() {
		item, err := scanPlannerHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

type handoffParent struct {
	expID, epoch, generation int64
	instance, status         string
	paused, queued           bool
}

func lockHandoffParent(ctx context.Context, tx *sql.Tx, taskID int64) (handoffParent, error) {
	var p handoffParent
	err := tx.QueryRowContext(ctx, `SELECT exploration_id,planner_handoff_epoch,planner_contract_generation,planner_handoff_instance_id,status,paused,queued FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, taskID).Scan(&p.expID, &p.epoch, &p.generation, &p.instance, &p.status, &p.paused, &p.queued)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrPlannerHandoffParentGone
	}
	if err != nil {
		return p, err
	}
	var archived bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_archives WHERE task_id=$1 AND state NOT IN ('archive_failed'))`, taskID).Scan(&archived); err != nil {
		return p, err
	}
	if archived {
		return p, ErrPlannerHandoffParentGone
	}
	return p, nil
}

func (d *DB) BeginPlannerHandoff(ctx context.Context, taskID, expID int64, instanceID, runID string) (*PlannerHandoffLease, *PlannerHandoff, error) {
	if runID == "" || !handoffTextOK(runID, 128) {
		return nil, nil, fmt.Errorf("invalid planner run identity")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, false); err != nil {
		return nil, nil, err
	}
	p, err := lockHandoffParent(ctx, tx, taskID)
	if err != nil {
		return nil, nil, err
	}
	opts, err := plannerHandoffOptions(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if !opts.Save {
		return nil, nil, ErrPlannerHandoffDisabled
	}
	if p.expID != expID || p.instance != instanceID || instanceID == "" || p.paused || p.queued || IsTerminal(p.status) {
		return nil, nil, ErrPlannerHandoffStale
	}
	basis, err := currentPlannerHandoffBasis(ctx, tx, taskID)
	if err != nil {
		return nil, nil, err
	}
	current, err := scanPlannerHandoff(tx.QueryRowContext(ctx, `SELECT `+plannerHandoffColumns+` FROM planner_handoffs WHERE task_id=$1`, taskID))
	if err != nil {
		return nil, nil, err
	}
	if err = tx.QueryRowContext(ctx, `UPDATE tasks SET planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=$1 RETURNING planner_handoff_epoch`, taskID).Scan(&p.epoch); err != nil {
		return nil, nil, err
	}
	lease := &PlannerHandoffLease{TaskID: taskID, ExplorationID: expID, InstanceID: instanceID, RunID: runID, Epoch: p.epoch, ContractGeneration: p.generation, ContractFingerprint: basis.ContractFingerprint}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return lease, current, nil
}

func (d *DB) CommitPlannerHandoff(ctx context.Context, lease PlannerHandoffLease, requestID string, expectedVersion int64, content PlannerHandoffContent, generatedAt time.Time) (*PlannerHandoff, error) {
	generatedAt = generatedAt.UTC().Truncate(time.Microsecond)
	hash, err := PlannerHandoffContentHash(content)
	if err != nil {
		return nil, err
	}
	if requestID == "" || !handoffTextOK(requestID, 128) || expectedVersion < 0 || generatedAt.IsZero() {
		return nil, fmt.Errorf("invalid planner handoff commit identity or time")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = handoffGate(ctx, tx, false); err != nil {
		return nil, err
	}
	p, err := lockHandoffParent(ctx, tx, lease.TaskID)
	if err != nil {
		return nil, err
	}
	// Confirm a previously committed request before evaluating the now-expired
	// publishing lease. This is a receipt lookup, never a second publication.
	existing, err := scanPlannerHandoff(tx.QueryRowContext(ctx, `SELECT h.task_id,$2::bigint,h.version,h.request_id,h.instance_id,h.run_id,h.epoch,h.contract_generation,h.content,h.content_hash,h.generated_at,h.committed_at,h.checked_at FROM planner_handoff_history h WHERE task_id=$1 AND request_id=$3`, lease.TaskID, p.expID, requestID))
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.ContentHash != hash || existing.InstanceID != lease.InstanceID || existing.RunID != lease.RunID || existing.Epoch != lease.Epoch || existing.ContractGeneration != lease.ContractGeneration || !existing.GeneratedAt.Equal(generatedAt) {
			return nil, ErrPlannerHandoffConflict
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	opts, err := plannerHandoffOptions(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !opts.Save {
		return nil, ErrPlannerHandoffDisabled
	}
	if p.expID != lease.ExplorationID || p.instance != lease.InstanceID || p.epoch != lease.Epoch || p.generation != lease.ContractGeneration || p.paused || p.queued || IsTerminal(p.status) {
		return nil, ErrPlannerHandoffStale
	}
	basis, err := currentPlannerHandoffBasis(ctx, tx, lease.TaskID)
	if err != nil {
		return nil, err
	}
	if lease.ContractFingerprint != basis.ContractFingerprint || content.Basis.ContractFingerprint != lease.ContractFingerprint {
		return nil, ErrPlannerHandoffStale
	}
	current, err := scanPlannerHandoff(tx.QueryRowContext(ctx, `SELECT `+plannerHandoffColumns+` FROM planner_handoffs WHERE task_id=$1`, lease.TaskID))
	if err != nil {
		return nil, err
	}
	version := int64(0)
	if current != nil {
		version = current.Version
	}
	if version != expectedVersion {
		return nil, ErrPlannerHandoffConflict
	}
	if current != nil && current.ContentHash == hash {
		if err = tx.QueryRowContext(ctx, `UPDATE planner_handoffs SET checked_at=clock_timestamp() WHERE task_id=$1 RETURNING checked_at`, lease.TaskID).Scan(&current.CheckedAt); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE planner_handoff_history SET checked_at=$3 WHERE task_id=$1 AND version=$2`, lease.TaskID, current.Version, current.CheckedAt); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return current, nil
	}
	// A live round may retain at most one history window. Its receipt keys
	// therefore cannot be pruned while the same publishing lease is valid.
	var roundCommits int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM planner_handoff_history WHERE task_id=$1 AND epoch=$2 AND instance_id=$3 AND run_id=$4`, lease.TaskID, lease.Epoch, lease.InstanceID, lease.RunID).Scan(&roundCommits); err != nil {
		return nil, err
	}
	if roundCommits >= MaxPlannerHandoffCommitsPerRun {
		return nil, fmt.Errorf("%w: round content-commit limit is %d", ErrPlannerHandoffTooLarge, MaxPlannerHandoffCommitsPerRun)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	out, err := scanPlannerHandoff(tx.QueryRowContext(ctx, `INSERT INTO planner_handoffs(task_id,exploration_id,current_version,last_request_id,instance_id,run_id,epoch,contract_generation,content,content_hash,generated_at,committed_at,checked_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,clock_timestamp(),clock_timestamp()) ON CONFLICT(task_id) DO UPDATE SET exploration_id=EXCLUDED.exploration_id,current_version=EXCLUDED.current_version,last_request_id=EXCLUDED.last_request_id,instance_id=EXCLUDED.instance_id,run_id=EXCLUDED.run_id,epoch=EXCLUDED.epoch,contract_generation=EXCLUDED.contract_generation,content=EXCLUDED.content,content_hash=EXCLUDED.content_hash,generated_at=EXCLUDED.generated_at,committed_at=EXCLUDED.committed_at,checked_at=EXCLUDED.checked_at RETURNING `+plannerHandoffColumns, lease.TaskID, lease.ExplorationID, version+1, requestID, lease.InstanceID, lease.RunID, lease.Epoch, lease.ContractGeneration, string(raw), hash, generatedAt.UTC()))
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planner_handoff_history(task_id,version,request_id,instance_id,run_id,epoch,contract_generation,content,content_hash,generated_at,committed_at,checked_at) SELECT task_id,current_version,last_request_id,instance_id,run_id,epoch,contract_generation,content,content_hash,generated_at,committed_at,checked_at FROM planner_handoffs WHERE task_id=$1`, lease.TaskID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM planner_handoff_history WHERE task_id=$1 AND version<=$2`, lease.TaskID, out.Version-PlannerHandoffHistoryLimit); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("planner handoff commit confirmation unavailable: %w", err)
	}
	return out, nil
}

const plannerHandoffContractSQL = `WITH owner AS (SELECT id,exploration_id,goal,description,coverage_enabled FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL), context_tasks AS (
 SELECT id,exploration_id FROM owner
 UNION SELECT s.id,s.exploration_id FROM task_relations r JOIN tasks s ON s.id=r.source_task_id AND s.deleted_at IS NULL AND s.archived_at IS NULL WHERE r.task_id=$1
)
SELECT md5(jsonb_build_object(
 'task',(SELECT to_jsonb(owner) FROM owner),
 'goals',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',n.id,'exp',n.exploration_id,'payload',n.payload) ORDER BY n.id) FROM exploration_nodes n JOIN context_tasks c ON c.exploration_id=n.exploration_id WHERE n.kind='goal'),'[]'::jsonb),
 'constraints',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',r.id,'exp',r.exploration_id,'kind',r.kind,'text',r.text) ORDER BY r.id) FROM task_constraints r JOIN context_tasks c ON c.exploration_id=r.exploration_id),'[]'::jsonb),
 'scope',COALESCE((SELECT jsonb_agg(to_jsonb(s)-'created_at'-'updated_at' ORDER BY s.id) FROM task_scope s JOIN context_tasks c ON c.id=s.task_id),'[]'::jsonb),
 'sources',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',r.source_task_id,'live',s.deleted_at IS NULL AND s.archived_at IS NULL,'goal',s.goal,'description',s.description) ORDER BY r.source_task_id) FROM task_relations r LEFT JOIN tasks s ON s.id=r.source_task_id WHERE r.task_id=$1),'[]'::jsonb)
)::text),
md5(jsonb_build_object(
 'nodes',COALESCE((SELECT string_agg(n.id::text || ':' || n.content_version::text || ':' || n.state || ':' || md5(n.payload::text),'|' ORDER BY n.id) FROM exploration_nodes n JOIN context_tasks c ON c.exploration_id=n.exploration_id WHERE n.kind IN ('fact','finding','intent') AND (c.id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))),''),
 'findings',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',f.id,'evidence_version',f.evidence_version,'status',f.status) ORDER BY f.id) FROM findings f JOIN context_tasks c ON c.id=f.task_id),'[]'::jsonb)
)::text)`

func currentPlannerHandoffBasis(ctx context.Context, q handoffQuerier, taskID int64) (PlannerHandoffBasis, error) {
	var out PlannerHandoffBasis
	// These are change detectors, never authorization or completion evidence.
	if err := q.QueryRowContext(ctx, plannerHandoffContractSQL, taskID).Scan(&out.ContractFingerprint, &out.EvidenceFingerprint); err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, `SELECT n.id,n.content_version FROM exploration_nodes n WHERE n.kind='goal' AND n.exploration_id IN (SELECT t.exploration_id FROM tasks t WHERE t.id=$1 AND t.deleted_at IS NULL AND t.archived_at IS NULL UNION SELECT t.exploration_id FROM task_relations r JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL WHERE r.task_id=$1) ORDER BY n.id LIMIT $2`, taskID, MaxPlannerHandoffReferences+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref PlannerHandoffReference
		ref.Kind = "node"
		ref.Available = true
		if err := rows.Scan(&ref.ID, &ref.Version); err != nil {
			return out, err
		}
		out.References = append(out.References, ref)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.References) > MaxPlannerHandoffReferences {
		return out, ErrPlannerHandoffTooLarge
	}
	return out, nil
}

// CurrentPlannerHandoffBasis reads current task/contract and evidence change
// detectors. No material from source-of-source tasks is traversed.
func (d *DB) CurrentPlannerHandoffBasis(ctx context.Context, taskID int64) (PlannerHandoffBasis, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return PlannerHandoffBasis{}, err
	}
	defer tx.Rollback()
	var alive bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&alive); err != nil {
		return PlannerHandoffBasis{}, err
	}
	if !alive {
		return PlannerHandoffBasis{}, ErrPlannerHandoffParentGone
	}
	out, err := currentPlannerHandoffBasis(ctx, tx, taskID)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func reconcileHandoffReference(ctx context.Context, q handoffQuerier, taskID int64, ref PlannerHandoffReference) (PlannerHandoffReference, error) {
	out := ref
	out.Available = false
	out.Reason = "引用已删除、退役或不再属于当前任务及直接来源"
	var version int64
	var raw string
	var retired bool
	query := `SELECT n.content_version,n.payload::text,COALESCE(n.payload->>'retired_contract','false')='true' OR n.state IN ('deleted','stopped') FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id AND t.deleted_at IS NULL AND t.archived_at IS NULL WHERE n.id=$2 AND (t.id=$1 OR EXISTS(SELECT 1 FROM task_relations r WHERE r.task_id=$1 AND r.source_task_id=t.id) AND (n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped')))`
	if ref.Kind == "finding" {
		query = `SELECT f.evidence_version,jsonb_build_object('name',f.name,'status',f.status,'evidence_version',f.evidence_version)::text,false FROM findings f JOIN tasks t ON t.id=f.task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL WHERE f.id=$2 AND (t.id=$1 OR EXISTS(SELECT 1 FROM task_relations r WHERE r.task_id=$1 AND r.source_task_id=t.id))`
	} else if ref.Kind != "node" {
		return out, fmt.Errorf("unsupported planner handoff reference kind")
	}
	err := q.QueryRowContext(ctx, query, taskID, ref.ID).Scan(&version, &raw, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if retired {
		return out, nil
	}
	if ref.Version != version {
		out.Reason = "引用版本已变化，须按当前图重新核对"
		return out, nil
	}
	if ref.Fingerprint != "" {
		sum := sha256.Sum256([]byte(raw))
		if ref.Fingerprint != hex.EncodeToString(sum[:]) {
			out.Reason = "引用内容已变化，须重新核对"
			return out, nil
		}
	}
	out.Available = true
	out.Reason = ""
	return out, nil
}

// ReconcilePlannerHandoff changes only a detached background projection. It
// cannot reopen work, alter evidence, or restore a completion/verification state.
func (d *DB) ReconcilePlannerHandoff(ctx context.Context, taskID int64, content PlannerHandoffContent) (PlannerHandoffContent, error) {
	if err := ValidatePlannerHandoffContent(content); err != nil {
		return content, err
	}
	// Detach all slices, preserving the caller's last committed snapshot.
	raw, err := json.Marshal(content)
	if err != nil {
		return content, err
	}
	var out PlannerHandoffContent
	if err = json.Unmarshal(raw, &out); err != nil {
		return content, err
	}
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return content, err
	}
	defer tx.Rollback()
	var alive bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&alive); err != nil {
		return content, err
	}
	if !alive {
		return content, ErrPlannerHandoffParentGone
	}
	current, err := currentPlannerHandoffBasis(ctx, tx, taskID)
	if err != nil {
		return content, err
	}
	invalid := out.Basis.ContractFingerprint != current.ContractFingerprint || out.Basis.EvidenceFingerprint != current.EvidenceFingerprint
	for i, ref := range out.Basis.References {
		checked, e := reconcileHandoffReference(ctx, tx, taskID, ref)
		if e != nil {
			return content, e
		}
		out.Basis.References[i] = checked
		invalid = invalid || !checked.Available
	}
	for i := range out.Todos {
		for j, ref := range out.Todos[i].References {
			checked, e := reconcileHandoffReference(ctx, tx, taskID, ref)
			if e != nil {
				return content, e
			}
			out.Todos[i].References[j] = checked
			if !checked.Available {
				out.Todos[i].NeedsReview = true
				invalid = true
			}
		}
	}
	if invalid {
		out.Decision = ""
		out.DeferredReason = ""
		out.ReconsiderWhen = ""
		out.NextDependencies = ""
		out.BackgroundMissing = true
		out.BackgroundGeneratedAt = nil
		for i := range out.Todos {
			out.Todos[i].NeedsReview = true
		}
	}
	return out, tx.Commit()
}

// Call only after taking the owner task row lock and before changing contract
// inputs. This affects handoff publication alone, never evidence settlement.
func bumpPlannerContract(ctx context.Context, tx *sql.Tx, taskID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET planner_contract_generation=planner_contract_generation+1,planner_handoff_epoch=planner_handoff_epoch+1 WHERE id=$1`, taskID)
	return err
}

func lockPlannerContractOwner(ctx context.Context, tx *sql.Tx, expID int64) (int64, error) {
	var taskID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, expID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return taskID, err
}

func (s *ExplorationStore) mutatePlannerContract(fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	taskID, err := lockPlannerContractOwner(ctx, tx, s.expID)
	if err != nil {
		return err
	}
	if err = fn(ctx, tx); err != nil {
		return err
	}
	if taskID > 0 {
		if err = bumpPlannerContract(ctx, tx, taskID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
