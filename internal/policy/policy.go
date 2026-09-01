// Package policy evaluates deterministic first-match-wins rules against a
// decision result. This is where mrmr's core invariant lives: the model
// recommends, the runtime decides. Everything here is pure evaluation —
// no I/O, no time, no randomness — so a given (rules, result) pair always
// produces the same outcome, which is what makes policy inspectable and
// trustworthy. Condition semantics are deliberately minimal (ANDed
// conditions, literal equality or a handful of operators, first match wins,
// no OR or nesting); anything richer is a later addition, not a v0 gap.
package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// Notify describes a notification outcome. Via names the adapter; v0.1
// ships only stdout. Message is a Go template executed over the decision
// result so flows can surface the model's own summary.
type Notify struct {
	Via     string `yaml:"via"`
	Message string `yaml:"message"` // Go template over {result: ...}; empty = summary default
}

// Then is the selected outcome for an event. Exactly one action is set;
// config validation enforces that, and the runtime trusts it.
type Then struct {
	Notify *Notify `yaml:"notify"`
	Ignore bool    `yaml:"ignore"`
}

func (t Then) Outcome() string {
	switch {
	case t.Notify != nil:
		return "notify"
	default:
		return "ignore"
	}
}

// Rule is one policy line: ANDed conditions and the outcome they select.
type Rule struct {
	If   map[string]any `yaml:"if"` // ANDed; value is literal or operator string
	Then Then           `yaml:"then"`
}

// Policy is the ordered rule list plus the fall-through default. Order is
// significant: rules evaluate top to bottom and the first match wins, so a
// policy file reads like the author's priority order.
type Policy struct {
	Rules   []Rule `yaml:"policy"`
	Default Then   `yaml:"default"`
}

// Evaluate returns the outcome of the first rule whose conditions all match,
// or the default. The second return value is the matched rule's index into
// Rules (0-based), or -1 when the default applied — the trace needs to say
// *which* rule fired, not just what it did, or "why did this happen?" is
// half-answered. A condition naming a field the result doesn't have simply
// doesn't match — missing data must fall through to less aggressive
// outcomes, never block them.
func (p Policy) Evaluate(result map[string]any) (Then, int) {
	for i, r := range p.Rules {
		if matchesAll(r.If, result) {
			return r.Then, i
		}
	}
	return p.Default, -1
}

func matchesAll(conds map[string]any, result map[string]any) bool {
	for key, want := range conds {
		path := strings.TrimPrefix(key, "result.")
		got, ok := result[path]
		if !ok {
			return false
		}
		if !match(want, got) {
			return false
		}
	}
	return true
}

// match compares a condition value against the actual result value. A
// string condition is either an operator expression ("> 0.8") or a literal
// for equality; anything else (numbers, booleans) is a literal. Comparison
// is by fmt.Sprint so YAML scalars and JSON values line up without a
// type-coercion matrix.
func match(want any, got any) bool {
	if w, ok := want.(string); ok {
		if op, operand, isOp := parseOperator(w); isOp {
			return compareOp(op, operand, got)
		}
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

var ops = []string{">=", "<=", "!=", ">", "<"}

// parseOperator recognizes "> x", ">= x", "< x", "<= x", "!= x".
func parseOperator(s string) (op, operand string, ok bool) {
	s = strings.TrimSpace(s)
	for _, o := range ops {
		if strings.HasPrefix(s, o) {
			return o, strings.TrimSpace(s[len(o):]), true
		}
	}
	return "", "", false
}

// compareOp applies an operator. Numeric comparison requires both sides to
// parse as numbers; ordering operators on non-numbers are false rather than
// an error, which keeps evaluation pure and total. "!" is the one operator
// that also makes sense for strings.
func compareOp(op, operand string, got any) bool {
	g, err1 := strconv.ParseFloat(fmt.Sprint(got), 64)
	w, err2 := strconv.ParseFloat(operand, 64)
	numeric := err1 == nil && err2 == nil
	switch op {
	case "!=":
		if numeric {
			return g != w
		}
		return fmt.Sprint(got) != operand
	case ">":
		return numeric && g > w
	case ">=":
		return numeric && g >= w
	case "<":
		return numeric && g < w
	case "<=":
		return numeric && g <= w
	}
	return false
}
