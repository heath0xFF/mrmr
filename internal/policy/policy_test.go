// These tests pin the deterministic half of the mrmr loop: policy.Evaluate
// must depend only on the decision result and rule order, never on model
// judgment. Everything the interpreter produces flows through here, so a
// regression in matching, ordering, or the default fallback silently
// changes what outcomes users see.
package policy

import "testing"

func notify() Then         { return Then{Notify: &Notify{Via: "stdout"}} }
func ignore() Then         { return Then{Ignore: true} }
func isNotify(t Then) bool { return t.Outcome() == "notify" }

func eval(t testing.TB, p Policy, result map[string]any) Then {
	then, _ := p.Evaluate(result)
	return then
}

func TestEvaluateOperators(t *testing.T) {
	tests := []struct {
		name string
		op   string
		got  any
		want bool
	}{
		{"gt true", "> 0.8", 0.9, true},
		{"gt false at boundary", "> 0.8", 0.8, false},
		{"gt false below", "> 0.8", 0.1, false},
		{"gte true at boundary", ">= 0.8", 0.8, true},
		{"gte true above", ">= 0.8", 0.81, true},
		{"gte false below", ">= 0.8", 0.79, false},
		{"lt true", "< 0.5", 0.4, true},
		{"lt false at boundary", "< 0.5", 0.5, false},
		{"lte true at boundary", "<= 0.5", 0.5, true},
		{"lte false above", "<= 0.5", 0.51, false},
		{"neq numeric false when equal", "!= 5", 5, false},
		{"neq numeric true when different", "!= 5", 6, true},
		{"neq numeric string operand", "!= 5", "6", true},
		{"neq non-numeric strings", "!= urgent", "normal", true},
		{"neq non-numeric equal strings", "!= urgent", "urgent", false},
		{"gt non-numeric operand never matches", "> high", "high", false},
		{"gt non-numeric result never matches", "> 0.5", "high", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Policy{
				Rules:   []Rule{{If: map[string]any{"result.score": tt.op}, Then: notify()}},
				Default: ignore(),
			}
			if got := isNotify(eval(t, p, map[string]any{"score": tt.got})); got != tt.want {
				t.Errorf("Evaluate(score=%v, cond %q) notify=%v, want %v", tt.got, tt.op, got, tt.want)
			}
		})
	}
}

func TestEvaluateLiteralEquality(t *testing.T) {
	p := Policy{
		Rules:   []Rule{{If: map[string]any{"result.category": "important"}, Then: notify()}},
		Default: ignore(),
	}
	if !isNotify(eval(t, p, map[string]any{"category": "important"})) {
		t.Error("string literal equality should match")
	}
	if isNotify(eval(t, p, map[string]any{"category": "unimportant"})) {
		t.Error("different string literal should not match")
	}
}

func TestEvaluateFirstMatchWins(t *testing.T) {
	// Both rules match; the earlier one must win. Rule order is the only
	// tiebreaker — policy has no priorities or scores.
	p := Policy{
		Rules: []Rule{
			{If: map[string]any{"result.category": "important"}, Then: ignore()},
			{If: map[string]any{"result.importance": "> 0"}, Then: notify()},
		},
		Default: notify(),
	}
	got := eval(t, p, map[string]any{"category": "important", "importance": 0.9})
	if isNotify(got) {
		t.Error("first matching rule should win (ignore), got notify")
	}
}

func TestEvaluateDefaultFallback(t *testing.T) {
	p := Policy{
		Rules:   []Rule{{If: map[string]any{"result.importance": "> 0.8"}, Then: notify()}},
		Default: ignore(),
	}
	if isNotify(eval(t, p, map[string]any{"importance": 0.2})) {
		t.Error("no rule matched; default (ignore) should apply")
	}
}

func TestEvaluateBoundaryAtThreshold(t *testing.T) {
	// Regression: importance exactly at the configured threshold must
	// match the rule, not fall through to default. This was broken when
	// the policy used strict greater-than ("> 0.8") because local models
	// commonly return 0.8 for actionable events.
	p := Policy{
		Rules: []Rule{{
			If:   map[string]any{"result.importance": ">= 0.8", "result.requires_action": true},
			Then: notify(),
		}},
		Default: ignore(),
	}
	if !isNotify(eval(t, p, map[string]any{"importance": 0.8, "requires_action": true})) {
		t.Error("importance at exact threshold with requires_action should notify, got default ignore")
	}
}

func TestEvaluateMissingResultFieldDoesNotMatch(t *testing.T) {
	// A condition naming a field the model never returned must fail the
	// rule, not panic and not silently match — otherwise typos in rule
	// keys would fire outcomes on every event.
	p := Policy{
		Rules:   []Rule{{If: map[string]any{"result.nonexistent": "> 0"}, Then: notify()}},
		Default: ignore(),
	}
	if isNotify(eval(t, p, map[string]any{"importance": 0.9})) {
		t.Error("condition on missing field must not match")
	}
}

func TestEvaluateResultPrefixStripping(t *testing.T) {
	// Conditions canonically use the "result." prefix, but the prefix is
	// stripped so bare field names behave identically. Both spellings
	// must resolve to the same result key.
	p := Policy{
		Rules: []Rule{
			{If: map[string]any{"result.category": "important"}, Then: notify()},
		},
		Default: ignore(),
	}
	if !isNotify(eval(t, p, map[string]any{"category": "important"})) {
		t.Error(`"result." prefixed key should strip the prefix`)
	}

	bare := Policy{
		Rules:   []Rule{{If: map[string]any{"category": "important"}, Then: notify()}},
		Default: ignore(),
	}
	if !isNotify(eval(t, bare, map[string]any{"category": "important"})) {
		t.Error("bare key (no prefix) should match the same field")
	}
}

func TestEvaluateReturnsMatchedRuleIndex(t *testing.T) {
	// The trace must be able to say which rule fired, so Evaluate reports
	// the matched rule's index — or -1 when the default applied. A trace
	// that says "notify" without saying why is half an answer.
	p := Policy{
		Rules: []Rule{
			{If: map[string]any{"result.importance": "> 0.9"}, Then: notify()},
			{If: map[string]any{"result.importance": "> 0.5"}, Then: ignore()},
		},
		Default: notify(),
	}
	if _, i := p.Evaluate(map[string]any{"importance": 0.95}); i != 0 {
		t.Errorf("importance 0.95 matched rule %d, want 0", i)
	}
	if _, i := p.Evaluate(map[string]any{"importance": 0.6}); i != 1 {
		t.Errorf("importance 0.6 matched rule %d, want 1", i)
	}
	if _, i := p.Evaluate(map[string]any{"importance": 0.1}); i != -1 {
		t.Errorf("no match returned %d, want -1 (default)", i)
	}
}

func TestEvaluateMultipleConditionsANDed(t *testing.T) {
	p := Policy{
		Rules: []Rule{{
			If:   map[string]any{"result.category": "important", "result.importance": "> 0.8"},
			Then: notify(),
		}},
		Default: ignore(),
	}
	if !isNotify(eval(t, p, map[string]any{"category": "important", "importance": 0.9})) {
		t.Error("all conditions satisfied should match")
	}
	if isNotify(eval(t, p, map[string]any{"category": "important", "importance": 0.5})) {
		t.Error("one unsatisfied condition must fail the whole rule (AND)")
	}
}
