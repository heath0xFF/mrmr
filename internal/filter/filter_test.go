// These tests pin the filter gate's contract: deterministic, fail
// toward ignore, value-free reasons. The cases that matter most are the
// bypass cases: a missing field must not pass neq, and a type-shifted
// value must not satisfy either operator.
package filter

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/event"
)

func evt(data, meta map[string]any) event.Event {
	return event.Event{
		Type:     "commit.pushed",
		Source:   "github",
		Subject:  "mrmr",
		Data:     data,
		Metadata: meta,
	}
}

func TestEvaluatePassAndFail(t *testing.T) {
	tests := []struct {
		name  string
		specs List
		e     event.Event
		want  bool
	}{
		{"neq passes a different value", List{{Field: "data.author", Op: OpNeq, Value: "dependabot[bot]"}}, evt(map[string]any{"author": "alice"}, nil), true},
		{"neq excludes an equal value", List{{Field: "data.author", Op: OpNeq, Value: "dependabot[bot]"}}, evt(map[string]any{"author": "dependabot[bot]"}, nil), false},
		{"empty op defaults to eq", List{{Field: "data.author", Value: "alice"}}, evt(map[string]any{"author": "alice"}, nil), true},
		{"eq excludes a different value", List{{Field: "data.author", Op: OpEq, Value: "alice"}}, evt(map[string]any{"author": "bob"}, nil), false},
		{"numbers normalize int vs float", List{{Field: "data.count", Value: 5}}, evt(map[string]any{"count": 5.0}, nil), true},
		{"numbers compare across representations", List{{Field: "data.count", Op: OpNeq, Value: 5.0}}, evt(map[string]any{"count": 4}, nil), true},
		{"bools compare as bools", List{{Field: "data.flag", Value: true}}, evt(map[string]any{"flag": true}, nil), true},
		{"top-level type", List{{Field: "type", Op: OpNeq, Value: "spam.ping"}}, evt(nil, nil), true},
		{"top-level source", List{{Field: "source", Value: "github"}}, evt(nil, nil), true},
		{"subject", List{{Field: "subject", Value: "mrmr"}}, evt(nil, nil), true},
		{"nested data path", List{{Field: "data.a.b", Value: "x"}}, evt(map[string]any{"a": map[string]any{"b": "x"}}, nil), true},
		{"metadata path", List{{Field: "metadata.source_event_id", Value: "gh-1"}}, evt(nil, map[string]any{"source_event_id": "gh-1"}), true},
		{"event prefix form", List{{Field: "event.data.author", Op: OpNeq, Value: "dependabot[bot]"}}, evt(map[string]any{"author": "alice"}, nil), true},
		{"empty list passes", List{}, evt(nil, nil), true},
		{"all specs must pass", List{{Field: "type", Value: "commit.pushed"}, {Field: "data.author", Value: "alice"}}, evt(map[string]any{"author": "bob"}, nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := tt.specs.Evaluate(tt.e)
			if got != tt.want {
				t.Fatalf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEvaluateMissingFieldFailsEveryOp pins the gate rule: a field the
// event does not carry fails the filter for every operator. In
// particular neq must not pass a missing field, or a malformed event
// bypasses the pre-model gate and reaches policy, which can authorize
// outcomes.
func TestEvaluateMissingFieldFailsEveryOp(t *testing.T) {
	for _, op := range []Op{"", OpEq, OpNeq} {
		t.Run("op="+string(op), func(t *testing.T) {
			specs := List{{Field: "data.author", Op: op, Value: "dependabot[bot]"}}
			if got, _ := specs.Evaluate(evt(nil, nil)); got {
				t.Fatal("Evaluate() = true for a missing field, want excluded")
			}
		})
	}
}

// TestEvaluateTypeMismatchFailsBothOps pins the second bypass rule: a
// type-shifted value is incomparable and fails both operators. The
// fmt.Sprint equality this package deliberately does not use would let
// true satisfy a "true" check and let a number slide past a string neq.
func TestEvaluateTypeMismatchFailsBothOps(t *testing.T) {
	tests := []struct {
		name  string
		specs List
		e     event.Event
	}{
		{"bool want, string got, neq", List{{Field: "data.flag", Op: OpNeq, Value: true}}, evt(map[string]any{"flag": "true"}, nil)},
		{"bool want, string got, eq", List{{Field: "data.flag", Value: true}}, evt(map[string]any{"flag": "true"}, nil)},
		{"string want, number got, neq", List{{Field: "data.author", Op: OpNeq, Value: "dependabot[bot]"}}, evt(map[string]any{"author": 5}, nil)},
		{"number want, string got, eq", List{{Field: "data.count", Value: 5}}, evt(map[string]any{"count": "5"}, nil)},
		{"number want, string got, neq", List{{Field: "data.count", Op: OpNeq, Value: 5}}, evt(map[string]any{"count": "5"}, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := tt.specs.Evaluate(tt.e); got {
				t.Fatal("Evaluate() = true for a type mismatch, want excluded")
			}
		})
	}
}

// TestEvaluateReasonIsValueFree pins the privacy rule: the failure
// reason reaches the trace, the HTTP response, and stdout, so it may
// carry the rule index, field path, and operator only. Event payload
// and configured values must never appear in it.
func TestEvaluateReasonIsValueFree(t *testing.T) {
	specs := List{{Field: "data.token", Op: OpNeq, Value: "expected-secret"}}
	e := evt(map[string]any{"token": "expected-secret"}, nil)
	got, reason := specs.Evaluate(e)
	if got {
		t.Fatal("Evaluate() = true, want excluded")
	}
	for _, leaked := range []string{"expected-secret"} {
		if strings.Contains(reason, leaked) {
			t.Errorf("reason %q leaks %q", reason, leaked)
		}
	}
	if reason != "filter 1: data.token (neq) did not match" {
		t.Errorf("reason = %q, want the value-free form", reason)
	}
}

// TestEvaluateReasonNamesTheFailingRule pins that the reason identifies
// which rule excluded the event, so the trace answers "why" without
// reading the config.
func TestEvaluateReasonNamesTheFailingRule(t *testing.T) {
	specs := List{
		{Field: "type", Value: "commit.pushed"},
		{Field: "metadata.source_event_id", Op: OpNeq, Value: "dup-1"},
	}
	e := evt(nil, map[string]any{"source_event_id": "dup-1"})
	got, reason := specs.Evaluate(e)
	if got {
		t.Fatal("Evaluate() = true, want excluded")
	}
	if reason != "filter 2: metadata.source_event_id (neq) did not match" {
		t.Errorf("reason = %q, want the second rule named", reason)
	}
}

func TestSpecValidate(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"empty field", Spec{Op: OpEq, Value: "x"}, "field is required"},
		{"unknown root", Spec{Field: "foo.bar", Op: OpEq, Value: "x"}, `unknown root "foo"`},
		{"bare data root", Spec{Field: "data", Op: OpEq, Value: "x"}, "data requires a sub-path"},
		{"bare metadata root", Spec{Field: "metadata", Op: OpEq, Value: "x"}, "metadata requires a sub-path"},
		{"sub-path under scalar root", Spec{Field: "type.x", Op: OpEq, Value: "x"}, "type has no sub-paths"},
		{"double event prefix", Spec{Field: "event.event.data.x", Op: OpEq, Value: "x"}, `unknown root "event"`},
		{"empty segment", Spec{Field: "data..x", Op: OpEq, Value: "x"}, "empty path segment"},
		{"trailing dot", Spec{Field: "data.x.", Op: OpEq, Value: "x"}, "empty path segment"},
		{"unknown op", Spec{Field: "data.author", Op: "lt", Value: "x"}, `op "lt" not supported`},
		{"missing value", Spec{Field: "data.author", Op: OpNeq, Value: nil}, "value is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want containing %q", err, tt.want)
			}
		})
	}
}

// TestSpecValidateAcceptsDocumentedForms pins the two documented
// spellings of a filter field (section 3 and the flow example) plus the
// scalar-root and metadata forms, so strict validation never rejects a
// documented example.
func TestSpecValidateAcceptsDocumentedForms(t *testing.T) {
	specs := []Spec{
		{Field: "data.author", Op: OpNeq, Value: "x"},
		{Field: "event.data.author", Op: OpNeq, Value: "x"},
		{Field: "event.type", Value: "x"},
		{Field: "metadata.source_event_id", Op: "", Value: 5},
	}
	for _, s := range specs {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", s, err)
		}
	}
}

// TestEvaluateEmptySubjectFailsNeq pins the optional-field rule:
// subject is optional, so an event without one has no value to
// compare. neq must not treat the empty string as a value that differs
// from the excluded one, or a subject-less event slips past the gate.
func TestEvaluateEmptySubjectFailsNeq(t *testing.T) {
	specs := List{{Field: "subject", Op: OpNeq, Value: "blocked"}}
	e := event.Event{Type: "commit.pushed", Source: "github"}
	if got, _ := specs.Evaluate(e); got {
		t.Fatal("Evaluate() = true for an empty subject, want excluded")
	}
}

// TestEvaluateEmptyScalarRootsFailNeq pins the same guard on type and
// source: Ingest can be called outside the HTTP handler, which is the
// only place that requires them non-empty, so an empty value there is
// not present, not a comparison.
func TestEvaluateEmptyScalarRootsFailNeq(t *testing.T) {
	for _, field := range []string{"type", "source", "subject"} {
		t.Run(field, func(t *testing.T) {
			specs := List{{Field: field, Op: OpNeq, Value: "blocked"}}
			if got, _ := specs.Evaluate(event.Event{}); got {
				t.Fatal("Evaluate() = true for an empty scalar root, want excluded")
			}
		})
	}
}

// TestSpecValidateRejectsNonFiniteFloat32 pins the config-side half of
// the non-finite rule for the float32 kind: a NaN or Inf config value
// can never match, so it must fail startup rather than silently
// exclude every event at runtime.
func TestSpecValidateRejectsNonFiniteFloat32(t *testing.T) {
	for _, v := range []any{float32(math.NaN()), float32(math.Inf(1))} {
		spec := Spec{Field: "data.score", Op: OpNeq, Value: v}
		if err := spec.Validate(); err == nil {
			t.Fatalf("Validate() = nil for %v, want a non-finite value error", v)
		}
	}
}

// TestEvaluateNonFiniteEventValueFails pins the event-side half: a NaN
// or Inf value in event data is not comparable, so it fails both
// operators. Without the guard, neq passes every non-finite value and
// malformed data slips past the gate.
func TestEvaluateNonFiniteEventValueFails(t *testing.T) {
	for _, v := range []any{math.NaN(), math.Inf(1)} {
		for _, op := range []Op{OpEq, OpNeq} {
			t.Run(fmt.Sprintf("%v-%s", v, op), func(t *testing.T) {
				specs := List{{Field: "data.score", Op: op, Value: 0.5}}
				if got, _ := specs.Evaluate(evt(map[string]any{"score": v}, nil)); got {
					t.Fatal("Evaluate() = true for a non-finite event value, want excluded")
				}
			})
		}
	}
}
