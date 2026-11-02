//go:build ignore

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTodoStoreDefaultKeepsLegacyReplacement(t *testing.T) {
	s := NewTodoStore()
	result, err := s.Tool().Call(context.Background(), json.RawMessage(`{"todos":[{"content":"unchanged behavior","status":"pending"}]}`), nil)
	if err != nil || result.Flatten() != todoAck {
		t.Fatalf("legacy ack = %q, %v", result.Flatten(), err)
	}
	if items := s.List(); len(items) != 1 || items[0].ID != "" || items[0].Content != "unchanged behavior" {
		t.Fatalf("legacy list = %+v", items)
	}
}

func TestTodoStorePolicyRejectsWithoutMutatingAndCopiesNestedReferences(t *testing.T) {
	s := NewTodoStore()
	initial := []Todo{{ID: "one", Content: "original", Status: TodoPending, References: []TodoReference{{ID: 7, Kind: "node", Version: 3, Available: true}}}}
	s.Restore(initial)
	initial[0].References[0].ID = 99
	got := s.List()
	got[0].References[0].ID = 88
	if id := s.List()[0].References[0].ID; id != 7 {
		t.Fatalf("references alias caller snapshot: %d", id)
	}
	denied := errors.New("invalid item")
	s.SetUpdatePolicy(func(previous, submitted []Todo) ([]Todo, error) {
		previous[0].References[0].ID = 66
		return nil, denied
	})
	_, err := s.Tool().Call(context.Background(), json.RawMessage(`{"todos":[{"content":"change","status":"pending"}]}`), nil)
	if !errors.Is(err, denied) {
		t.Fatalf("error=%v", err)
	}
	got = s.List()
	if got[0].Content != "original" || got[0].References[0].ID != 7 {
		t.Fatalf("rejected write mutated stored value: %+v", got)
	}
}

func TestTodoStoreEnhancedAcknowledgesMemoryOnlyAndReturnsIdentity(t *testing.T) {
	s := NewTodoStore()
	s.SetUpdatePolicy(func(_, submitted []Todo) ([]Todo, error) { submitted[0].ID = "host-id"; return submitted, nil })
	result, err := s.Tool().Call(context.Background(), json.RawMessage(`{"todos":[{"content":"inspect","status":"pending"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Flatten(); !strings.Contains(text, "memory") || !strings.Contains(text, "host-id") || !strings.Contains(text, "does not confirm") {
		t.Fatalf("misleading ack=%q", text)
	}
	schema, _ := json.Marshal(s.Tool().InputSchema())
	if !strings.Contains(string(schema), `"references"`) || !strings.Contains(string(schema), `"id"`) {
		t.Fatalf("enhanced schema=%s", schema)
	}
}
