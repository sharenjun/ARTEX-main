//go:build ignore

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func resumeRecord(message llm.Message) transcript.Record {
	return transcript.Record{Type: "message", Message: &message}
}
func resumeTool(id string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "side_effect"}}}
}
func resumeResult(id string, failed bool) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, "saved result", failed)}}
}

func TestWorkerResumeDistinguishesFreshRestoredCorruptAndUnknown(t *testing.T) {
	tests := []struct {
		name      string
		records   []transcript.Record
		corrupt   bool
		state     string
		wantError bool
	}{
		{name: "first run", state: "fresh"},
		{name: "saved human message and tool result", records: []transcript.Record{resumeRecord(llm.UserText("human direction")), resumeRecord(resumeTool("same")), resumeRecord(resumeResult("same", false))}, state: "restored"},
		{name: "error result is still known", records: []transcript.Record{resumeRecord(resumeTool("x")), resumeRecord(resumeResult("x", true))}, state: "restored"},
		{name: "reused id followed by missing result", records: []transcript.Record{resumeRecord(resumeTool("same")), resumeRecord(resumeResult("same", false)), resumeRecord(resumeTool("same"))}, state: "unknown", wantError: true},
		{name: "result before call does not confirm later effect", records: []transcript.Record{resumeRecord(resumeResult("same", false)), resumeRecord(resumeTool("same"))}, state: "unknown", wantError: true},
		{name: "corrupt transcript", corrupt: true, state: "degraded", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := transcript.NewStore(t.TempDir())
			if len(tc.records) > 0 {
				if err := store.Append(store.MainPath("stable"), tc.records...); err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				if err := os.WriteFile(store.MainPath("stable"), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			session := agentcore.NewSession(agentcore.Options{Transcript: store, SessionID: "stable", DisableBackgroundTasks: true})
			defer session.Close()
			state, err := resumeWorkerConversation(session, store, "stable")
			if state != tc.state || (err != nil) != tc.wantError {
				t.Fatalf("state=%q err=%v", state, err)
			}
			if tc.wantError {
				var history *llm.HistoryError
				if !errors.As(err, &history) {
					t.Fatalf("recovery error must prevent blind retry: %T", err)
				}
			}
			if state == "restored" && session.SessionID() != "stable" {
				t.Fatal("resume changed stable identity")
			}
			if tc.name == "saved human message and tool result" && (len(session.Messages()) != 3 || session.Messages()[0].Text() != "human direction") {
				t.Fatal("saved human input/result was lost")
			}
		})
	}
}

func TestWorkerRecoveryWriteFailureDoesNotClaimResultsUnexecuted(t *testing.T) {
	var activities []db.Activity
	emitWorkerRecovery(func(a db.Activity) { activities = append(activities, a) }, "write_failed", errors.New("disk full"))
	if len(activities) != 1 || !activities[0].IsError || !strings.Contains(activities[0].Detail, "已保存事实") || !strings.Contains(activities[0].Detail, "不能当作尚未执行") {
		t.Fatalf("misleading degradation: %+v", activities)
	}
}

func TestWorkerTranscriptWriteFailureIsObservable(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, event := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: "saved elsewhere"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
			if !yield(event, nil) {
				return
			}
		}
	}}
	session := agentcore.NewSession(agentcore.Options{Provider: provider, Transcript: transcript.NewStore(blocked), SessionID: "stable", DisableBackgroundTasks: true})
	defer session.Close()
	if _, _, err := captureRunSession(context.Background(), session, "run", nil); err != nil {
		t.Fatal(err)
	}
	if session.TranscriptError() == nil {
		t.Fatal("disk failure silently appeared durable")
	}
}

func TestWorkerFreshHumanInputDoesNotCountAsExecutedHistory(t *testing.T) {
	if priorWorkerExecution([]db.Activity{{Worker: "user", Kind: "user", Summary: "queued first message"}}) {
		t.Fatal("first human input was mistaken for executed side effect")
	}
	if !priorWorkerExecution([]db.Activity{{Worker: "user", Kind: "user"}, {Worker: "work#1", Kind: "tool_use"}}) {
		t.Fatal("tool execution history was ignored")
	}
}
