//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

type collectingFindingRecorder struct {
	inputs []db.RecordFindingInput
}

func (r *collectingFindingRecorder) Record(_ context.Context, in db.RecordFindingInput, _ []db.TrafficRef) (*db.RecordedFinding, error) {
	r.inputs = append(r.inputs, in)
	id := int64(len(r.inputs))
	return &db.RecordedFinding{FindingID: id, NodeID: 100 + id, Traffic: &db.FindingTraffic{}}, nil
}

func TestReportFindingKeepsIndependentFindingsOnSameAsset(t *testing.T) {
	// This exercises the real tool handler without requiring PostgreSQL. The
	// recorder deliberately receives all reports for one asset and one intent.
	recorder := &collectingFindingRecorder{}
	tools := NewToolSet(&db.ExplorationStore{}, "work#1")
	tools.SetTaskID(6)
	tools.SetFindingRecorder(recorder)
	var notified []string
	tools.notifyFinding = func(intentID int64, summary string) {
		if intentID != 8 {
			t.Fatalf("wrong intent notified: %d", intentID)
		}
		notified = append(notified, summary)
	}
	findings := []struct {
		class, summary, evidence string
	}{
		{"SQL injection", "/search query injection", "search response verifies query injection"},
		{"SQL injection", "/export filter injection", "export response verifies filter injection"},
		{"authorization", "/accounts cross-user access", "second user reads another account"},
	}
	seenFindingIDs, seenNodeIDs := map[int64]bool{}, map[int64]bool{}
	for _, finding := range findings {
		input, err := json.Marshal(map[string]any{
			"intent_id": 8, "asset_ids": []int64{42}, "severity": "high",
			"vulnclass": finding.class, "summary": finding.summary, "evidence": finding.evidence,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := tools.addFinding().Call(t.Context(), input, nil)
		if err != nil || result.IsError {
			t.Fatalf("independent report failed: %v %s", err, result.Flatten())
		}
		lines := strings.SplitN(result.Flatten(), "\n", 2)
		if len(lines) != 2 {
			t.Fatalf("missing independent finding handle: %s", result.Flatten())
		}
		var recorded db.RecordedFinding
		if err := json.Unmarshal([]byte(lines[1]), &recorded); err != nil {
			t.Fatal(err)
		}
		if recorded.FindingID <= 0 || recorded.NodeID <= 0 || seenFindingIDs[recorded.FindingID] || seenNodeIDs[recorded.NodeID] {
			t.Fatalf("same-asset finding reused a handle: %+v", recorded)
		}
		seenFindingIDs[recorded.FindingID], seenNodeIDs[recorded.NodeID] = true, true
		if lines[0] != fmt.Sprintf("finding recorded: %d", recorded.NodeID) {
			t.Fatalf("reporter lost exploration node handle: %s", lines[0])
		}
	}
	if len(recorder.inputs) != len(findings) || len(notified) != len(findings) || tools.Writes().Findings != len(findings) {
		t.Fatalf("lost same-asset report: recorded=%d notifications=%d writes=%d", len(recorder.inputs), len(notified), tools.Writes().Findings)
	}
	for i, input := range recorder.inputs {
		if input.TaskID != 6 || input.IntentID != 8 || len(input.AssetIDs) != 1 || input.AssetIDs[0] != 42 || input.VulnClass != findings[i].class || input.Summary != findings[i].summary || input.Evidence != findings[i].evidence || notified[i] != findings[i].summary {
			t.Fatalf("report %d merged independent evidence: %+v", i, input)
		}
	}
}
