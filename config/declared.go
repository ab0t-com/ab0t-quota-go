package config

// Declared[T] — the three-way config value (pack 20260721, GO-01/E-63;
// design_dependency_resolution_20260721.md §7.1). A plain `string` +
// omitempty collapses JSON null / absent / "" into one state, which is how
// an UNDECLARED counter store silently became a per-process counter.
// The property that gives three states: encoding/json invokes UnmarshalJSON
// only when the key is PRESENT — so the zero value means ABSENT.

import (
	"bytes"
	"encoding/json"
)

// Declared models a config value that distinguishes declared / explicit-null
// / absent. The zero value means ABSENT.
type Declared[T any] struct {
	set  bool // key was present in the JSON document
	null bool // key was present and was JSON null
	val  T
}

// Declare returns a Declared carrying an explicit value — the Go-code
// equivalent of writing the key in quota-config.json.
func Declare[T any](v T) Declared[T] { return Declared[T]{set: true, val: v} }

// DeclareNull returns a Declared carrying an explicit JSON null.
func DeclareNull[T any]() Declared[T] { return Declared[T]{set: true, null: true} }

func (d *Declared[T]) UnmarshalJSON(b []byte) error {
	d.set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		d.null = true
		return nil
	}
	return json.Unmarshal(b, &d.val)
}

// MarshalJSON emits null for both absent and explicit-null. Stated honestly:
// Go's omitempty cannot suppress an absent struct field, and the config is
// read-mostly (only tests marshal it) — acceptable per design §7.1.
func (d Declared[T]) MarshalJSON() ([]byte, error) {
	if !d.set || d.null {
		return []byte("null"), nil
	}
	return json.Marshal(d.val)
}

// IsAbsent reports the key was never in the document.
func (d Declared[T]) IsAbsent() bool { return !d.set }

// IsNull reports the key was present and explicitly null.
func (d Declared[T]) IsNull() bool { return d.set && d.null }

// Get returns the declared value and whether one was declared (set, non-null).
func (d Declared[T]) Get() (T, bool) {
	if d.set && !d.null {
		return d.val, true
	}
	var zero T
	return zero, false
}
