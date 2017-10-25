// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package unify

import (
	"testing"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/util/test"
)

func TestUnifyValues(t *testing.T) {

	tests := []struct {
		note  string
		a     string
		b     string
		exp   string
		undos int
	}{
		{"nulls", `null`, `null`, "null", 1},
		{"nulls", `null`, `x`, "null", 1},
		{"booleans", `true`, `false`, "", 0},
		{"booleans", `true`, `true`, "true", 1},
		{"booleans", `true`, `x`, "true", 1},
		{"booleans", `false`, `x`, "false", 1},
		{"numbers", `1`, `2`, "", 0},
		{"numbers", `1`, `1`, "1", 1},
		{"numbers", `1`, `x`, "1", 1},
		{"strings", `"a"`, `"b"`, "", 0},
		{"strings", `"a"`, `"a"`, `"a"`, 1},
		{"strings", `"a"`, `b`, `"a"`, 1},
		{"strings", `"b"`, `a`, `"b"`, 1},
		{"arrays", `[]`, `["a"]`, "", 0},
		{"arrays", `["a"]`, `["a"]`, `["a"]`, 1},
		{"arrays", `["a", b, "c"]`, `[x, "y", z]`, `["a", "y", "c"]`, 3},
		{"arrays", `["a", b, b]`, `[x, x, "z"]`, "", 0},
		{"arrays", `[a, b, c]`, `[x, y, z]`, "[x, y, z]", 3},
		{"arrays", `[a, b, b]`, `[x, y, x]`, "[x, x, x]", 3},
		{"objects", `{}`, `{"a": 1}`, "", 0},
		{"objects", `{"a": 1, "b": 2}`, `{"a": 1}`, "", 0},
		{"objects", `{"a": 1, "b": 2}`, `{"a": 1, "b": 2}`, `{"a": 1, "b": 2}`, 2},
		{"objects", `{"a": x, "b": 2}`, `{"a": 1, "b": 2}`, `{"a": 1, "b": 2}`, 2},
		{"objects", `{"a": x, "b": x}`, `{"a": 1, "b": 2}`, ``, 0},
		{"sets", `{1, 2}`, `{1,}`, "", 0},
		{"sets", `{1, 2}`, `{1, 2}`, "{1,2}", 1},
		{"sets", `{1, 2}`, `{1, x}`, "", 0},
	}

	for _, tc := range tests {
		test.Subtest(t, tc.note, func(t *testing.T) {
			a := New()
			b := New()
			termA := ast.MustParseTerm(tc.a)
			termB := ast.MustParseTerm(tc.b)
			changes := Unify(termA, a, termB, b)
			if tc.exp == "" && changes != nil {
				t.Fatalf("Expected unify(%v, %v) to fail but got:\n\na:\n\n%v\n\nb:\n\n%v", termA, termB, a, b)
			} else if tc.exp != "" {
				if changes == nil {
					t.Fatalf("Expected unify(%v, %v) to succeed", termA, termB)
				}
				exp := ast.MustParseTerm(tc.exp)
				r1 := a.Plug(termA)
				r2 := b.Plug(termB)
				if !exp.Equal(r1) || !exp.Equal(r2) {
					t.Fatalf("Expected unify(%v, %v) => %v but got: %v and %v", termA, termB, exp, r1, r2)
				}
				i := 0
				u := changes.(*undo)
				for u != nil {
					i++
					u, _ = u.next.(*undo)
				}
				if i != tc.undos {
					t.Fatalf("Expected unify(%v, %v) to produce %d undos but got %d", termA, termB, tc.undos, i)
				}
			}
		})
	}
}

func TestUnifyMultiple(t *testing.T) {
	a, b := New(), New()

	Unify(term("x"), a, term("[y, [z]]"), b)
	Unify(term("z"), b, term("2"), b)
	Unify(term("y"), b, term("1"), b)

	result := a.Plug(term("x"))
	exp := term("[1, [2]]")

	if !result.Equal(exp) {
		t.Fatalf("Expected %v but got %v", exp, result)
	}
}

func TestUnifyMultipleNamespaced(t *testing.T) {
	a, b, c, d, e := New(), New(), New(), New(), New()

	Unify(term("[x, 1]"), a, term("x"), b)
	Unify(term("x"), b, term("x"), c)
	Unify(term("x"), c, term("[x, 1]"), d)
	Unify(term("[x, _]"), d, term("[1,1]"), e)

	result := a.Plug(term("x"))
	exp := term("1")

	if !result.Equal(exp) {
		t.Fatalf("Expected %v but got %v", exp, result)
	}
}

func TestUnifySame(t *testing.T) {
	a := New()
	undo := Unify(term("x"), a, term("x"), a)
	result := a.Plug(term("x"))
	exp := term("x")
	if !result.Equal(exp) {
		t.Fatalf("Expected %v but got %v", exp, result)
	}
	UndoAll(undo)
}

func TestUnifierZeroValues(t *testing.T) {
	var unifier *Unifier

	// Plugging
	result := unifier.Plug(term("x"))
	exp := term("x")
	if !result.Equal(exp) {
		t.Fatalf("Expected %v but got %v", exp, result)
	}

	// String
	if unifier.String() != "{}" {
		t.Fatalf("Expected empty string but got: %v", unifier.String())
	}
}

func term(s string) *ast.Term {
	return ast.MustParseTerm(s)
}
