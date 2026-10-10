//go:build ignore

package guard

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	_ "modernc.org/sqlite"
)

func guardPolicyDB(t *testing.T, action string) (*db.DB, *intercept.Interceptor) {
	t.Helper()
	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	d := &db.DB{DB: pool}
	guardPolicyExec(t, d, `CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`)
	guardPolicyExec(t, d, `CREATE TABLE intercept_rules (
		id INTEGER PRIMARY KEY, name TEXT, enabled BOOLEAN, priority INTEGER,
		match_target TEXT, match_type TEXT, pattern TEXT, action TEXT, message TEXT,
		timeout_enabled BOOLEAN, timeout_seconds INTEGER, timeout_action TEXT,
		created_at DATETIME, updated_at DATETIME)`)
	guardPolicyExec(t, d, `CREATE TABLE conversations (id INTEGER PRIMARY KEY, title TEXT, agent_key TEXT)`)
	guardPolicyExec(t, d, `CREATE TABLE intercept_pending (
		id INTEGER PRIMARY KEY, rule_id INTEGER, conversation_id INTEGER, task_id TEXT,
		agent_name TEXT, tool_name TEXT, tool_input TEXT, status TEXT DEFAULT 'pending',
		reason TEXT, decided_at DATETIME, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		decision_source TEXT, audit TEXT)`)
	if action != "" {
		guardPolicyExec(t, d, `INSERT INTO intercept_rules VALUES
			(1, 'offline probe', true, 100, 'tool_name', 'contains', 'Bash', ?, 'offline policy',
			false, 0, 'deny', ?, ?)`, action, time.Now(), time.Now())
	}
	return d, intercept.New(d)
}

func guardPolicyExec(t *testing.T, d *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func assertTerminalAudit(t *testing.T, g *Guard, blocked bool, reason string) {
	t.Helper()
	entries := g.Audit()
	if len(entries) != 1 {
		t.Fatalf("one completed call must have one terminal audit entry, got %+v", entries)
	}
	action := "allow"
	if blocked {
		action = "block"
	}
	if entries[0].Action != action || entries[0].Reason != reason || entries[0].Tool != "Bash" || entries[0].Command != "printf offline-probe" {
		t.Fatalf("terminal decision or original input lost: %+v", entries[0])
	}
}

var guardProbeInput = []byte(`{"command":"printf offline-probe"}`)

func TestGuardClosedDatabaseFailsClosed(t *testing.T) {
	for _, state := range []string{"first load", "invalidated cache", "complete cache"} {
		t.Run(state, func(t *testing.T) {
			d, ic := guardPolicyDB(t, "deny")
			if state != "first load" {
				if enabled, err := ic.IsToolEnabledWithError("Bash"); err != nil || !enabled {
					t.Fatalf("fixture failed to load: %v %v", enabled, err)
				}
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if state == "invalidated cache" {
				ic.Invalidate()
			}
			g := NewWithInterceptor(ic)
			blocked, reason, _ := g.Hooks().PreToolUse(t.Context(), "Bash", guardProbeInput)
			if !blocked || reason == "" {
				t.Fatalf("closed policy database allowed execution: %v %q", blocked, reason)
			}
			if state != "complete cache" && !strings.Contains(reason, "load intercept rules") {
				t.Fatalf("load failure missing diagnostic cause: %s", reason)
			}
			assertTerminalAudit(t, g, blocked, reason)
		})
	}
}

func TestGuardConfigurationFailuresFailClosed(t *testing.T) {
	for _, broken := range []string{"settings read", "tool JSON", "judge read"} {
		t.Run(broken, func(t *testing.T) {
			d, ic := guardPolicyDB(t, "")
			if broken == "judge read" {
				if _, err := ic.IsToolEnabledWithError("Bash"); err != nil {
					t.Fatal(err)
				}
			}
			if broken == "tool JSON" {
				guardPolicyExec(t, d, `INSERT INTO settings VALUES ('intercept_enabled_tools', 'malformed')`)
			} else {
				guardPolicyExec(t, d, `DROP TABLE settings`)
			}
			g := NewWithInterceptor(ic)
			blocked, reason, _ := g.Hooks().PreToolUse(t.Context(), "Bash", guardProbeInput)
			if !blocked || !strings.Contains(reason, "加载失败") {
				t.Fatalf("unreadable configuration allowed execution: %v %q", blocked, reason)
			}
			assertTerminalAudit(t, g, blocked, reason)
		})
	}
}

func TestGuardAllowAndDenyHaveOneTerminalAudit(t *testing.T) {
	for _, policy := range []string{"deny", "allow", "", "disabled"} {
		t.Run(policy, func(t *testing.T) {
			action := policy
			if action == "disabled" {
				action = "deny"
			}
			d, ic := guardPolicyDB(t, action)
			if policy == "disabled" {
				guardPolicyExec(t, d, `INSERT INTO settings VALUES ('intercept_enabled_tools', '[]')`)
			}
			g := NewWithInterceptor(ic)
			blocked, reason, _ := g.Hooks().PreToolUse(t.Context(), "Bash", guardProbeInput)
			if blocked != (policy == "deny") {
				t.Fatalf("wrong decision: blocked=%v policy=%q", blocked, policy)
			}
			assertTerminalAudit(t, g, blocked, reason)
		})
	}
}

func TestGuardModelVerdictHasOneTerminalAudit(t *testing.T) {
	for _, action := range []string{"allow", "deny"} {
		t.Run(action, func(t *testing.T) {
			d, ic := guardPolicyDB(t, "")
			guardPolicyExec(t, d, `INSERT INTO settings VALUES ('llm_judge_enabled', 'true')`)
			reviews := 0
			ic.SetReviewer(func(context.Context, int64, string, intercept.ReviewInput) (intercept.Decision, error) {
				reviews++
				return intercept.Decision{Action: action, Message: "offline review"}, nil
			})
			g := NewWithInterceptor(ic)
			blocked, reason, _ := g.Hooks().PreToolUse(t.Context(), "Bash", guardProbeInput)
			if blocked != (action == "deny") || reviews != 1 {
				t.Fatalf("wrong fallback decision: blocked=%v reviews=%d action=%s", blocked, reviews, action)
			}
			assertTerminalAudit(t, g, blocked, reason)
		})
	}
}

func TestGuardSavedHumanDecisionHasOneTerminalAudit(t *testing.T) {
	for _, status := range []string{"allowed", "denied"} {
		t.Run(status, func(t *testing.T) {
			d, ic := guardPolicyDB(t, "ask")
			// Reproduce a persisted human decision arriving between the pending
			// INSERT and channel registration. HandleAsk must honor the saved result.
			guardPolicyExec(t, d, `CREATE TRIGGER saved_decision AFTER INSERT ON intercept_pending
				BEGIN UPDATE intercept_pending SET status='`+status+`', decided_at=CURRENT_TIMESTAMP
				WHERE id=NEW.id; END`)
			g := NewWithInterceptor(ic)
			blocked, reason, _ := g.Hooks().PreToolUse(t.Context(), "Bash", guardProbeInput)
			if blocked != (status == "denied") {
				t.Fatalf("wrong saved human decision: blocked=%v status=%s", blocked, status)
			}
			assertTerminalAudit(t, g, blocked, reason)
		})
	}
}

func TestGuardPendingApprovalHasNoPrematureAllow(t *testing.T) {
	_, ic := guardPolicyDB(t, "ask")
	g := NewWithInterceptor(ic)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pending := make(chan struct{}, 1)
	ctx = intercept.WithTaskContext(ctx, "offline-task", "worker", func(db.Activity) {
		pending <- struct{}{}
	})
	type outcome struct {
		blocked bool
		reason  string
	}
	finished := make(chan outcome, 1)
	go func() {
		blocked, reason, _ := g.Hooks().PreToolUse(ctx, "Bash", guardProbeInput)
		finished <- outcome{blocked, reason}
	}()
	select {
	case <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("approval never entered pending state")
	}
	if entries := g.Audit(); len(entries) != 0 {
		t.Fatalf("pending approval already recorded a terminal decision: %+v", entries)
	}
	cancel()
	select {
	case result := <-finished:
		if !result.blocked {
			t.Fatal("cancelled approval allowed execution")
		}
		assertTerminalAudit(t, g, result.blocked, result.reason)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled approval did not finish")
	}
}
