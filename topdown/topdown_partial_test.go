// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package topdown

import (
	"context"
	"fmt"
	"testing"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/storage/inmem"
	"github.com/open-policy-agent/opa/util"
	"github.com/open-policy-agent/opa/util/test"
)

func TestTopDownPartialEval(t *testing.T) {

	saveInput := []string{`input`}

	tests := []struct {
		note     string
		query    string
		modules  []string
		data     string
		input    string
		expected []string
	}{
		{
			note:     "empty",
			query:    "true = true",
			expected: []string{``},
		},
		{
			note:     "query vars",
			query:    "x = 1",
			expected: []string{`x = 1`},
		},
		{
			note:     "trivial",
			query:    "input.x = 1",
			expected: []string{`input.x = 1`},
		},
		{
			note:     "transitive",
			query:    "input.x = y; y[0] = z; z = 1",
			expected: []string{`input.x = y; y[0] = z; z = 1`},
		},
		{
			note:  "iterate data",
			query: "data.x[i] = input.x",
			data:  `{"x": [1,2,3]}`,
			expected: []string{
				`input.x = 1; i = 0`,
				`input.x = 2; i = 1`,
				`input.x = 3; i = 2`,
			},
		},
		{
			note:  "iterate rules: partial object",
			query: `data.test.p[x] = input.x`,
			modules: []string{
				`package test

				p["a"] = "b"
				p["b"] = "c"
				p["c"] = "d"`,
			},
			expected: []string{
				`"b" = input.x; x = "a"`,
				`"c" = input.x; x = "b"`,
				`"d" = input.x; x = "c"`,
			},
		},
		{
			note:  "iterate rules: partial set",
			query: `data.test.p[x]; input.x = x`,
			modules: []string{
				`package test

				p[1]
				p[2]
				p[3]`,
			},
			expected: []string{
				`input.x = 1; x = 1`,
				`input.x = 2; x = 2`,
				`input.x = 3; x = 3`,
			},
		},
		{
			note:  "namespace",
			query: "data.test.p[[x,y]]; input.y = x",
			modules: []string{
				`package test

				p[[y,x]] { x = 2; y = input.x }`,
			},
		},
		{
			note:  "complete",
			query: "data.test.p = input.x",
			modules: []string{
				`package test

				p = x { x = "foo" }`,
			},
			expected: []string{
				`"foo" = input.x`,
			},
		},
		{
			note:  "complete-namespace",
			query: "data.test.p = x; x = input.y",
			modules: []string{
				`package test

				p = x { input.x = x }`,
			},
			expected: []string{
				`input.x = x; x = x; x = input.y`,
			},
		},
		{
			note:  "both",
			query: "input.x = input.y",
			expected: []string{
				`input.x = input.y`,
			},
		},
		{
			note:  "transitive",
			query: "input.x = x; x[0] = y; x = z; y = 1; z = 2",
		},
		{
			note:  "call",
			query: "input.a = a; data.test.f(a) = b; b[0] = c",
			modules: []string{
				`package test

				f(x) = [y] {
					x = 1
					y = x
				}

				f(x) = [y] {
					x = 2
					y = 3
				}`,
			},
		},
	}

	ctx := context.Background()

	for _, tc := range tests {
		params := fixtureParams{
			note:    tc.note,
			query:   tc.query,
			modules: tc.modules,
			data:    tc.data,
			input:   tc.input,
		}
		prepareTest(ctx, t, params, func(ctx context.Context, t *testing.T, f fixture) {

			partial := make([]*ast.Term, len(saveInput))
			for i := range saveInput {
				partial[i] = ast.MustParseTerm(saveInput[i])
			}

			query := NewQuery(f.query).
				WithCompiler(f.compiler).
				WithStore(f.store).
				WithTransaction(f.txn).
				WithInput(f.input).
				WithPartial(partial)

			partials, err := query.PartialRun(ctx)

			if err != nil {
				t.Fatal(err)
			}

			expected := make([]ast.Body, len(tc.expected))
			for i := range tc.expected {
				expected[i] = ast.MustParseBody(tc.expected[i])
			}

			a, b := bodySet(partials), bodySet(expected)
			if !a.Equal(b) {
				missing := b.Diff(a)
				extra := a.Diff(b)
				t.Fatalf("Partial evaluation results differ. Expected %d but got %d:\nMissing: %v\nExtra: %v", len(b), len(a), missing, extra)
			}
		})
	}
}

type bodySet []ast.Body

func (s bodySet) Contains(b ast.Body) bool {
	for i := range s {
		if s[i].Equal(b) {
			return true
		}
	}
	return false
}

func (s bodySet) Diff(other bodySet) (r bodySet) {
	for i := range s {
		if !other.Contains(s[i]) {
			r = append(r, s[i])
		}
	}
	return r
}

func (s bodySet) Equal(other bodySet) bool {
	return len(s.Diff(other)) == 0 && len(other.Diff(s)) == 0
}

type fixtureParams struct {
	note    string
	data    string
	modules []string
	query   string
	input   string
}

type fixture struct {
	query    ast.Body
	compiler *ast.Compiler
	store    storage.Store
	txn      storage.Transaction
	input    *ast.Term
}

func prepareTest(ctx context.Context, t *testing.T, params fixtureParams, f func(context.Context, *testing.T, fixture)) {

	test.Subtest(t, params.note, func(t *testing.T) {

		var store storage.Store

		if len(params.data) > 0 {
			j := util.MustUnmarshalJSON([]byte(params.data))
			store = inmem.NewFromObject(j.(map[string]interface{}))
		} else {
			store = inmem.New()
		}

		storage.Txn(ctx, store, storage.TransactionParams{}, func(txn storage.Transaction) error {

			compiler := ast.NewCompiler()
			modules := map[string]*ast.Module{}

			for i, module := range params.modules {
				modules[fmt.Sprint(i)] = ast.MustParseModule(module)
			}

			if compiler.Compile(modules); compiler.Failed() {
				t.Fatal(compiler.Errors)
			}

			var input *ast.Term
			if len(params.input) > 0 {
				input = ast.MustParseTerm(params.input)
			}

			queryContext := ast.NewQueryContext()
			if input != nil {
				queryContext = queryContext.WithInput(input.Value)
			}

			queryCompiler := compiler.QueryCompiler().WithContext(queryContext)

			compiledQuery, err := queryCompiler.Compile(ast.MustParseBody(params.query))
			if err != nil {
				t.Fatal(err)
			}

			f(ctx, t, fixture{
				query:    compiledQuery,
				compiler: compiler,
				store:    store,
				txn:      txn,
				input:    input,
			})

			return nil
		})
	})
}

func toTerm(qrs QueryResultSet) *ast.Term {
	set := &ast.Set{}
	for _, qr := range qrs {
		obj := ast.Object{}
		for k, v := range qr {
			if !k.IsWildcard() {
				obj = append(obj, ast.Item(ast.NewTerm(k), v))
			}
		}
		set.Add(ast.NewTerm(obj))
	}
	return ast.NewTerm(set)
}
