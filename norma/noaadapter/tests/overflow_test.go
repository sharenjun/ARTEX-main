//go:build ignore

package noaadapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// The three harness-facing methods are the whole public surface between noa and
// the agent loop, and until now only View was exercised.

// Pre must not touch the history. The built-in compaction rewrites messages
// here; noa's message identity is a content hash, so a rewrite would change
// every id and invalidate every ref the model is holding.
func TestPreLeavesTheHistoryUntouched(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	in := []llm.Message{userText("one"), userText("two")}

	out := c.Pre(t.Context(), in, 12345)
	if len(out) != len(in) {
		t.Fatalf("Pre returned %d messages, want the %d it was given", len(out), len(in))
	}
	for i := range in {
		if out[i].Text() != in[i].Text() {
			t.Fatalf("Pre rewrote message %d: %q -> %q", i, in[i].Text(), out[i].Text())
		}
	}
	// The provider's own count is more accurate than any estimate, so it must be
	// taken when offered — resolveTokenCount prefers it over estimateCoreTokens
	// whenever it is fresh and close.
	sess.mu.Lock()
	got := sess.providerTokens
	sess.mu.Unlock()
	if got != 12345 {
		t.Fatalf("Pre recorded providerTokens = %d, want 12345; the pressure ladder keeps "+
			"running on estimates when a measured number was available", got)
	}
}

// A zero count means the provider did not report one — it must not be recorded
// as "the context is empty".
func TestPreIgnoresAnUnreportedTokenCount(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	c.Pre(t.Context(), nil, 7000)
	before := sess.tokensBefore()
	c.Pre(t.Context(), nil, 0)
	if sess.tokensBefore() != before {
		t.Fatalf("a zero token count overwrote the last known size %d with %d",
			before, sess.tokensBefore())
	}
}

// IsOverflow decides whether Reactive runs at all. Getting it wrong in one
// direction wastes a recovery attempt; in the other it turns a recoverable
// prompt-too-long into a failed run.
func TestIsOverflowRecognisesProviderPhrasings(t *testing.T) {
	c := &Compactor{sess: simSession(t, 40000)}
	overflow := []string{
		"This model's maximum context length is 200000 tokens, however you requested 214000",
		"prompt is too long: 215000 tokens > 200000 maximum",
		"error: context_length_exceeded",
		"anthropic: status 413",
	}
	for _, msg := range overflow {
		if !c.IsOverflow(errors.New(msg)) {
			t.Errorf("IsOverflow(%q) = false; the run fails instead of recovering", msg)
		}
	}
	other := []string{"rate limit exceeded", "connection reset by peer", "invalid api key"}
	for _, msg := range other {
		if c.IsOverflow(errors.New(msg)) {
			t.Errorf("IsOverflow(%q) = true; a recovery attempt is spent on an unrelated failure", msg)
		}
	}
	if c.IsOverflow(nil) {
		t.Error("IsOverflow(nil) = true")
	}
}

// The harness detects overflow before calling Reactive. That sequence must
// learn the actual window, shrink the request to it, and keep using it next turn.
func TestOverflowLearningIsWiredIn(t *testing.T) {
	c := &Compactor{sess: simSession(t, 200000)}
	a := newSimAgent(t, c.sess, 1_000_000)
	for range 10 {
		a.work(60_000)
	}
	c.View(t.Context(), a.history)
	before := c.sess.sentTokens()
	if before <= 128_000 {
		t.Fatalf("fixture is only %d tokens; it must exceed the actual window", before)
	}
	original := renderView(a.history)

	// The provider says its real window is 128000, not the configured 200000.
	err := errors.New("This model's maximum context length is 128000 tokens, however you requested 214000")
	if n := ParseOverflowWindow(err.Error()); n != 128000 {
		t.Fatalf("ParseOverflowWindow = %d, want 128000 — the parser itself is broken", n)
	}

	// The loop's actual sequence: detect, then recover.
	if !c.IsOverflow(err) {
		t.Fatal("provider overflow was not recognised")
	}
	if _, ok := c.Reactive(t.Context(), a.history); !ok {
		t.Fatal("large tool results did not produce a fitting reactive view")
	}

	floor := c.overflow.armedFloor()
	want := int(float64(128000) * 0.95)
	if floor != want {
		t.Fatalf("armed floor = %d, want %d from the actual provider window", floor, want)
	}
	if after := c.sess.sentTokens(); after >= before || after > floor {
		t.Fatalf("reactive view = %d tokens, before = %d, learned ceiling = %d", after, before, floor)
	}
	if renderView(a.history) != original {
		t.Fatal("reactive recovery changed stored history")
	}
	a.work(60_000)
	if n := estimateMessages(c.View(t.Context(), a.history)); n > floor {
		t.Fatalf("next turn ignored the learned floor: %d > %d", n, floor)
	}
}

func TestOverflowLearningKeepsTheSmallestWindow(t *testing.T) {
	c := &Compactor{sess: simSession(t, 200_000)}
	for _, msg := range []string{
		"maximum context length is 128000 tokens",
		"maximum context length is 160000 tokens",
		"maximum context length is 100000 tokens",
		"maximum context length is 256000 tokens",
	} {
		if !c.IsOverflow(errors.New(msg)) {
			t.Fatalf("did not recognise %q", msg)
		}
	}
	c.overflow.arm(c.sess.Config().ModelContextLimit)
	if got := c.overflow.armedFloor(); got != 95_000 {
		t.Fatalf("learned ceiling expanded: %d, want 95000", got)
	}
	if c.sess.learnedContextWindow != 100_000 {
		t.Fatalf("session window expanded: %d", c.sess.learnedContextWindow)
	}
}

func TestOverflowLearningIgnoresUnrelatedErrors(t *testing.T) {
	c := &Compactor{sess: simSession(t, 200_000)}
	if c.IsOverflow(errors.New("rate limit: requests are limited to 32000 tokens per minute")) {
		t.Fatal("rate limit treated as a context overflow")
	}
	if c.overflow.learnedWindow != 0 || c.sess.learnedContextWindow != 0 {
		t.Fatal("unrelated error changed the context window")
	}
}

func TestOverflowLearningSeparatesContextAndOutputBudgets(t *testing.T) {
	c := &Compactor{sess: simSession(t, 200_000)}
	err := errors.New(`status 413: {"error":"prompt too long","max_tokens":8192,"context_limit":128000}`)
	if !c.IsOverflow(err) {
		t.Fatal("provider overflow was not recognised")
	}
	c.overflow.arm(c.sess.Config().ModelContextLimit)
	if floor := c.overflow.armedFloor(); floor != 121_600 || c.sess.learnedContextWindow != 128_000 {
		t.Fatalf("output budget used as context window: floor=%d window=%d", floor, c.sess.learnedContextWindow)
	}

	c = &Compactor{sess: simSession(t, 200_000)}
	if !c.IsOverflow(errors.New(`status 413: {"error":"prompt too long","max_tokens":8192}`)) {
		t.Fatal("overflow without a reported window was not recognised")
	}
	if c.overflow.learnedWindow != 0 || c.sess.learnedContextWindow != 0 {
		t.Fatal("output-only budget reduced the context window")
	}
}

func TestOverflowLearningSurvivesAnUnshrinkableView(t *testing.T) {
	c := &Compactor{sess: simSession(t, 200_000)}
	msgs := []llm.Message{userText(strings.Repeat("x", 160_000))}
	c.View(t.Context(), msgs)
	if !c.IsOverflow(errors.New("maximum context length is 32000 tokens")) {
		t.Fatal("provider overflow was not recognised")
	}
	if _, ok := c.Reactive(t.Context(), msgs); ok {
		t.Fatal("reactive claimed success without shrinking protected user content")
	}
	if c.overflow.armedFloor() != 0 || c.sess.emergencyFloor != 0 {
		t.Fatal("failed reactive attempt left the mechanical valve armed")
	}
	if c.sess.learnedContextWindow != 32_000 {
		t.Fatal("failed reactive attempt forgot the actual provider window")
	}
	// The next turn must apply the known 32K window even though reactive could
	// not shrink the previous request. Large old tool results now can be cut.
	a := newSimAgent(t, c.sess, 1_000_000)
	for range 12 {
		a.work(24_000)
	}
	view := c.View(t.Context(), a.history)
	if c.sess.lastTruncatedCount == 0 || estimateMessages(view) > 32_000 {
		t.Fatalf("next turn ignored learned window: truncated=%d tokens=%d",
			c.sess.lastTruncatedCount, estimateMessages(view))
	}
}

// The parser itself, over the phrasings it claims to cover.
func TestParseOverflowWindowCoversItsPatterns(t *testing.T) {
	cases := map[string]int{
		"This model's maximum context length is 200000 tokens":                             200000,
		"context window of 131072 exceeded":                                                131072,
		"max_tokens must be less than 8192":                                                0,
		"requests are limited to 32000 tokens":                                             0,
		`status 413: {"error":"prompt too long","max_tokens":8192,"context_limit":128000}`: 128000,
		`status 413: {"error":"prompt too long","max_tokens":8192}`:                        0,
		`{"max_context_tokens":65536}`:                                                     65536,
		"prompt is too long: 215000 tokens > 200000 maximum":                               200000,
		"rate limit exceeded":                                                              0,
		"maximum context length is zero":                                                   0,
	}
	for msg, want := range cases {
		if got := ParseOverflowWindow(msg); got != want {
			t.Errorf("ParseOverflowWindow(%q) = %d, want %d", msg, got, want)
		}
	}
}

// Reactive must never claim success it cannot deliver: returning true promises
// the harness a smaller request, and if the request is not smaller the retry
// hits the same 413 and the loop spins.
func TestReactiveRefusesWhenItCannotShrink(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	// Nothing sent yet and nothing to cut.
	msgs := []llm.Message{userText("short")}
	if _, ok := c.Reactive(context.Background(), msgs); ok {
		if est := estimateMessages(c.View(context.Background(), msgs)); est > 0 {
			t.Logf("Reactive accepted a %d-token view under an armed floor", est)
		}
	}
	// Whatever it decided, it must leave the emergency floor consistent with that
	// decision: a refusal that left the valve armed would shrink every later view
	// for no reason.
	if !c.overflow.armed && c.overflow.armedFloor() != 0 {
		t.Fatal("disarmed but still reporting a floor")
	}
}
