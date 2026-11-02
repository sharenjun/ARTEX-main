//go:build ignore

package server

import (
	"encoding/json"
	"strconv"
	"testing"

	pgdb "github.com/Autumn-27/artex/db"
)

func TestTaskArchiveRejectsInvalidHandoffBeforeInstallingPayload(t *testing.T) {
	t.Parallel()
	for _, version := range []int{3, pgdb.TaskArchiveFormatVersion} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			// No manager, traffic store or filesystem is configured. Invalid
			// handoff metadata must fail before any external restore operation.
			snapshot := &pgdb.TaskArchiveSnapshot{
				FormatVersion: version, TaskID: 42, ExplorationID: 73,
				Tables: map[string]json.RawMessage{
					"planner_handoffs":        json.RawMessage(`[{"task_id":99,"exploration_id":73,"current_version":1}]`),
					"planner_handoff_history": json.RawMessage(`[{"task_id":99,"version":1}]`),
				},
			}
			if err := (&Server{}).restoreTaskArchivePayload(nil, snapshot, t.TempDir()); err == nil {
				t.Fatal("corrupt or incompatible handoff package reached external restore")
			}
		})
	}
}
