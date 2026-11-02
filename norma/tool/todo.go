package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/permission"
)

// TodoStatus is the state of a todo item.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// Todo is one task in the agent's working plan.
type Todo struct {
	Content      string          `json:"content"`
	Status       TodoStatus      `json:"status"`
	ID           string          `json:"id,omitempty"`
	References   []TodoReference `json:"references,omitempty"`
	NeedsReview  bool            `json:"needs_review,omitempty"`
	ReviewReason string          `json:"review_reason,omitempty"`
}

// TodoReference identifies an existing business node without copying its data.
// Availability and Reason are host observations, never authority to alter it.
type TodoReference struct {
	Kind        string `json:"kind"`
	ID          int64  `json:"id"`
	Version     int64  `json:"version"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
}

// TodoUpdatePolicy optionally validates and assigns identity to a replacement.
// It runs under the store lock and must not perform I/O or call this store.
// Returning an error leaves the current list intact.
type TodoUpdatePolicy func(previous, submitted []Todo) ([]Todo, error)

// TodoStore holds the current todo list across tool calls. It is safe for
// concurrent reads; the host can inspect progress via List.
type TodoStore struct {
	mu     sync.Mutex
	items  []Todo
	policy TodoUpdatePolicy
}

// NewTodoStore returns an empty store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// List returns a snapshot of the current todos.
func (s *TodoStore) List() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTodos(s.items)
}

func (s *TodoStore) set(items []Todo) {
	s.Restore(items)
}

// Restore loads a host-validated snapshot without treating it as a tool write.
func (s *TodoStore) Restore(items []Todo) {
	s.mu.Lock()
	s.items = cloneTodos(items)
	s.mu.Unlock()
}

// SetUpdatePolicy enables optional host-controlled identity and validation.
// An unset policy retains the original in-memory whole-list replacement.
func (s *TodoStore) SetUpdatePolicy(policy TodoUpdatePolicy) {
	s.mu.Lock()
	s.policy = policy
	s.mu.Unlock()
}

func (s *TodoStore) replace(items []Todo) ([]Todo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	enhanced := s.policy != nil
	items = cloneTodos(items)
	if enhanced {
		var err error
		items, err = s.policy(cloneTodos(s.items), items)
		if err != nil {
			return nil, true, err
		}
	}
	s.items = cloneTodos(items)
	return cloneTodos(s.items), enhanced, nil
}

func cloneTodos(items []Todo) []Todo {
	out := append([]Todo(nil), items...)
	for i := range out {
		out[i].References = append([]TodoReference(nil), out[i].References...)
	}
	return out
}

// Tool returns the TodoWrite tool bound to this store. The agent uses it to
// plan and track multi-step work; each call replaces the whole list.
func (s *TodoStore) Tool() CoreTool {
	s.mu.Lock()
	enhanced := s.policy != nil
	s.mu.Unlock()
	properties := map[string]any{
		"content": map[string]any{"type": "string"},
		"status":  map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
	}
	prompt := "status is one of: pending, in_progress, completed. Keep the list current: mark a task completed as soon as it's done; don't batch completions."
	if enhanced {
		properties["id"] = map[string]any{"type": "string", "description": "Keep the same id when renaming or reordering an existing item; omit for a new item."}
		properties["references"] = map[string]any{
			"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "integer"}, "kind": map[string]any{"type": "string", "enum": []any{"node", "finding"}},
				"version": map[string]any{"type": "integer"}, "fingerprint": map[string]any{"type": "string"},
			}, "required": []any{"id", "kind", "version"}},
		}
		prompt += " Keep stable item ids across whole-list replacement. References identify only already-read nodes and versions. Success updates memory only; persistence is reported separately by the host."
	}
	return Build(Spec{
		Name:        "TodoWrite",
		Description: "Records and updates a structured todo list for the current task. Use it to plan multi-step work and to mark progress — replace the whole list each call, marking exactly one item in_progress while working.",
		Prompt:      prompt,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":       "object",
						"properties": properties,
						"required":   []any{"content", "status"},
					},
				},
			},
			"required": []any{"todos"},
		},
		ReadOnly:   func(json.RawMessage) bool { return false },
		Concurrent: func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, input json.RawMessage, _ *ToolContext) (Result, error) {
			var in struct {
				Todos []Todo `json:"todos"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return Result{}, err
			}
			items, enhanced, err := s.replace(in.Todos)
			if err != nil {
				return Result{}, err
			}
			if enhanced {
				return Text("Todo list updated in memory; this does not confirm a durable commit. Stable items:\n" + RenderTodoReminder(items)), nil
			}
			// Ack only — the list itself is visible in the model's own tool_use
			// input and is re-surfaced on demand via the todo system-reminder
			// (harness). Mirrors claude-code's TodoWrite result.
			return Text(todoAck), nil
		},
	})
}

const todoAck = "Todos have been modified successfully. Keep the todo list current and continue with the tasks."

// RenderTodoReminder renders the todo list for the system-reminder body, one
// numbered item per line ("N. [status] content"). Returns "" for an empty list.
func RenderTodoReminder(todos []Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range todos {
		if t.ID != "" {
			fmt.Fprintf(&b, "%d. [%s] id=%s %s", i+1, t.Status, t.ID, t.Content)
			if t.NeedsReview {
				fmt.Fprintf(&b, " (needs review: %s)", t.ReviewReason)
			}
			b.WriteByte('\n')
		} else {
			fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, t.Status, t.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
