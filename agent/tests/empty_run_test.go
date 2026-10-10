//go:build ignore

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestEmptyModelRunCannotCompleteTask(t *testing.T) {
	for _, output := range []llm.StreamEvent{
		{Type: llm.SETextDelta, Text: " \n"},
		{Type: llm.SEThinkingDelta, Text: "thinking only"},
	} {
		p := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 11}}, output,
				{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 2}},
				{Type: llm.SEMessageStop},
			} {
				if !yield(ev, nil) {
					return
				}
			}
		}}
		var activities []db.Activity
		_, reason, err := captureRun(context.Background(), agentcore.Options{Provider: p, MaxTurns: 1}, "plan", func(a db.Activity) {
			activities = append(activities, a)
		})
		if !errors.Is(err, errEmptyModelResponse) || reason != harness.ReasonModelError {
			t.Fatalf("reason=%q err=%v, want model_error", reason, err)
		}
		assertCapturedResultUsage(t, activities, 11, 2, 0, 0)
		for _, a := range activities {
			if a.Kind == "result" && !a.IsError {
				t.Fatalf("empty run recorded as success: %+v", a)
			}
			if a.Kind == "result" && !strings.Contains(a.Summary, "未返回") {
				t.Fatalf("empty run omitted its diagnostic: %+v", a)
			}
		}
	}
}

func TestCaptureAllowsSilentStopAfterTool(t *testing.T) {
	turn, calls := 0, 0
	p := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		turn++
		events := []llm.StreamEvent{{Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}}
		if turn == 1 {
			events = []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "write-1", ToolName: "WritePlanProbe"},
				{Type: llm.SEToolInputJSON, Text: "{}"}, {Type: llm.SEMessageDelta, StopReason: "tool_use"},
			}
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	probe := tool.Build(tool.Spec{Name: "WritePlanProbe", Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			calls++
			return tool.Text("plan persisted"), nil
		},
	})
	s := agentcore.NewSession(agentcore.Options{Provider: p, Tools: []tool.CoreTool{probe}, PermissionMode: permission.ModeBypass, MaxTurns: 3})
	defer s.Close()
	if _, reason, err := captureRunSession(context.Background(), s, "write plan", nil); err != nil || reason != harness.ReasonCompleted || calls != 1 {
		t.Fatalf("silent tool stop: reason=%q err=%v calls=%d", reason, err, calls)
	}
	// Earlier work must not make a later, entirely empty Prompt appear productive.
	if _, reason, err := captureRunSession(context.Background(), s, "new plan", nil); !errors.Is(err, errEmptyModelResponse) || reason != harness.ReasonModelError {
		t.Fatalf("empty subsequent prompt: reason=%q err=%v", reason, err)
	}
}
