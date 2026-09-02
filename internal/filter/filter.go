// Package filter applies deterministic checks to Events before any model
// call. It is the pipeline's cost gate (IMPLEMENTATION.md section 3):
// events that ordinary code can already exclude must not spend inference.
// Evaluation is pure: no I/O, no clock, no model. A filter can never
// fail in a way a model can, which is what makes the excluded path safe
// by construction.
//
// Two rules shape this package.
//
// Fail toward ignore. The filter is the only gate before policy, and
// policy is what authorizes outcomes. An event that cannot be resolved
// (missing field, malformed path, incomparable type) fails every
// comparison and is excluded, never passed by default. A malformed
// event that slips past the gate is one good model day from an outcome
// nobody authorized.
//
// Reasons are value-free. Failure reasons flow into the trace, the HTTP
// response, and stdout, and event payloads may be sensitive. A reason
// carries the rule index, field path, and operator only.
//
// This package does not reuse policy's comparisons on purpose. Filter
// ops are named (eq, neq) and compared by strict semantic type; policy
// conditions are operator-expression strings compared by fmt.Sprint.
// The gate is where a type-shifted value becomes a bypass, so it must
// not inherit the looser semantics. If a future third caller really
// shares one of these semantics, extract a neutral condition package
// then, not before.
package filter

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/heath0xff/mrmr/internal/event"
)

// Op is the comparison a Spec applies. v0.1 ships only the two
// comparisons the spec documents for filters: neq (excluding known
// senders such as bots) and eq, which is also the default when op is
// left empty. Numeric ordering lands when a real flow needs it; the
// Spec shape does not change.
type Op string

const (
	OpEq  Op = "eq"
	OpNeq Op = "neq"
)

// Spec is one filter line. All Specs in a List are ANDed, mirroring
// policy's condition semantics: alternatives are separate Specs, not OR.
type Spec struct {
	Field string `yaml:"field"`
	Op    Op     `yaml:"op"`
	Value any    `yaml:"value"`
}

// List is the ordered set of Specs. An empty list passes every event, so
// a config without a filter section behaves exactly as before.
type List []Spec

// fieldRoots are the only path roots a Spec may address. They are the
// Event's top-level fields, with data and metadata open to dot descent.
var fieldRoots = map[string]bool{
	"type":     true,
	"source":   true,
	"subject":  true,
	"data":     true,
	"metadata": true,
}

// normalizePath reduces a Spec's field to its canonical form and reports
// whether it addresses a value the runtime can compare. The spec
// documents both "data.author" (section 3) and "event.data.author" (the
// flow example); exactly one leading "event." is accepted, so both forms
// behave identically. Everything else is rejected at config time because
// a filter that can never match is a typo, not a choice: unknown roots,
// a bare data/metadata (a map can never compare), and sub-paths under
// scalar roots (a scalar can never have children).
func normalizePath(field string) (string, error) {
	if field == "" {
		return "", fmt.Errorf("field is required")
	}
	if strings.HasPrefix(field, "event.") {
		field = field[len("event."):]
	}
	parts := strings.Split(field, ".")
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("field %q: empty path segment", field)
		}
	}
	root := parts[0]
	if !fieldRoots[root] {
		return "", fmt.Errorf("field %q: unknown root %q (want type, source, subject, data, or metadata)", field, root)
	}
	switch root {
	case "type", "source", "subject":
		if len(parts) > 1 {
			return "", fmt.Errorf("field %q: %s has no sub-paths", field, root)
		}
	default:
		if len(parts) == 1 {
			return "", fmt.Errorf("field %q: %s requires a sub-path", field, root)
		}
	}
	return field, nil
}

// Validate reports whether the Spec can run correctly. Config calls it
// for every filter line, so a malformed filter refuses startup rather
// than mis-firing at request time.
func (s Spec) Validate() error {
	if _, err := normalizePath(s.Field); err != nil {
		return err
	}
	switch s.Op {
	case "", OpEq, OpNeq:
	default:
		return fmt.Errorf("op %q not supported (v0.1: eq, neq)", s.Op)
	}
	if s.Value == nil {
		return fmt.Errorf("field %q: value is required", s.Field)
	}
	if !valueOK(s.Value) {
		return fmt.Errorf("field %q: value must be a string, number, or boolean", s.Field)
	}
	return nil
}

// Evaluate reports whether e passes every Spec in order, and returns the
// first failure as a value-free reason (rule index, path, operator) for
// the trace. ok is true when the list is empty or every Spec passes.
func (l List) Evaluate(e event.Event) (bool, string) {
	for i, s := range l {
		path, err := normalizePath(s.Field)
		if err != nil {
			// Unreachable after config validation; fail closed anyway.
			// A Spec that cannot be parsed must exclude, not pass.
			return false, fmt.Sprintf("filter %d: malformed field", i+1)
		}
		got, present := resolve(e, path)
		if !present {
			// A missing field fails every operator, including neq: the
			// event cannot prove itself acceptable to the gate, and
			// exclusion is the only safe answer.
			return false, fmt.Sprintf("filter %d: field %s not present", i+1, path)
		}
		if !matches(s.Op, s.Value, got) {
			return false, fmt.Sprintf("filter %d: %s (%s) did not match", i+1, path, opName(s.Op))
		}
	}
	return true, ""
}

func opName(op Op) string {
	if op == "" {
		return string(OpEq)
	}
	return string(op)
}

// resolve walks a normalized dot path against the Event. Scalar roots
// resolve exactly; data and metadata descend into maps. Every miss
// (unknown key, a non-map mid-path, a map at the end, an empty scalar
// root) is "not present": resolution never guesses. Subject is
// optional, so an empty subject is not present. The same guard applies
// to type and source because Ingest can be called outside the HTTP
// handler, which is the only place that requires them non-empty.
func resolve(e event.Event, path string) (any, bool) {
	parts := strings.Split(path, ".")
	switch parts[0] {
	case "type":
		if e.Type != "" {
			return e.Type, true
		}
	case "source":
		if e.Source != "" {
			return e.Source, true
		}
	case "subject":
		if e.Subject != "" {
			return e.Subject, true
		}
	case "data":
		return walk(e.Data, parts[1:])
	case "metadata":
		return walk(e.Metadata, parts[1:])
	}
	return nil, false
}

// walk descends one level at a time. It is only called with a non-empty
// parts slice: normalizePath rejects a bare data/metadata root.
func walk(m map[string]any, parts []string) (any, bool) {
	cur, ok := m[parts[0]]
	if !ok {
		return nil, false
	}
	if len(parts) == 1 {
		return cur, true
	}
	next, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	return walk(next, parts[1:])
}

// kind is a value's semantic type. Anything that is not a number,
// string, or bool (nil, maps, slices) is opaque and comparable with
// nothing.
type kind int

const (
	kindNumber kind = iota
	kindString
	kindBool
	kindOpaque
)

func classify(v any) kind {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return kindNumber
	case string:
		return kindString
	case bool:
		return kindBool
	}
	return kindOpaque
}

// toFloat normalizes a numeric value to float64, reporting whether the
// result is usable for a comparison. Non-finite inputs (NaN, Inf) are
// not comparable: NaN never equals itself, so an eq check can never
// match and a neq check would pass every value, malformed data
// included. They are reported incomparable instead of compared.
func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return f, finite(f)
	}
	return 0, false // classify() guarantees a number here
}

// valueOK reports whether a Spec value can ever take part in a
// comparison. The comparator works on three semantic types: number,
// string, bool. Anything else (lists, maps, channels) can never
// match, and a filter line that can never match is a typo, not a
// choice: it must fail startup, not silently exclude every event at
// runtime. Numbers must be finite: NaN never equals itself, so it can
// never satisfy eq and would pass neq against every value; Inf is not
// a value a source will send. Both are unmatchable by construction.
func valueOK(v any) bool {
	switch n := v.(type) {
	case string, bool:
		return true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return finite(float64(n))
	case float64:
		return finite(n)
	}
	return false
}

// finite reports whether a number can take part in a comparison.
// Shared by valueOK (config side) and toFloat (event side) so the
// non-finite rule has one definition.
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// compare reports whether two values are equal, and whether they are
// comparable at all. Comparison is strict by semantic type on purpose,
// and intentionally stricter than policy.match's fmt.Sprint semantics:
// at the gate, a type-shifted value (the number 5 where a string is
// expected, "true" where a bool is expected) must not satisfy neq and
// slide past the gate. Numeric values normalize across int and float
// representations because YAML decodes 5 as int and JSON decodes it as
// float64: the same number, two sources. Values of different kinds are
// incomparable.
func compare(want, got any) (equal, comparable bool) {
	k := classify(want)
	if k != classify(got) || k == kindOpaque {
		return false, false
	}
	switch k {
	case kindNumber:
		w, wok := toFloat(want)
		g, gok := toFloat(got)
		if !wok || !gok {
			return false, false
		}
		return w == g, true
	case kindString:
		return want.(string) == got.(string), true
	case kindBool:
		return want.(bool) == got.(bool), true
	}
	return false, false
}

// matches applies the operator. An incomparable pair fails for both eq
// and neq: the event is excluded either way, and the trace says which
// check excluded it.
func matches(op Op, want, got any) bool {
	equal, comparable := compare(want, got)
	if !comparable {
		return false
	}
	if op == OpNeq {
		return !equal
	}
	return equal
}
