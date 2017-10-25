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

func TestEvalDot(t *testing.T) {
	tests := []struct {
		note     string
		data     string
		modules  []string
		input    string
		given    string
		ref      string
		expected string
	}{
		{
			note:     "defined",
			data:     `{"foo": {"bar": [{"baz": 0}]}}`,
			ref:      `data.foo.bar[0].baz`,
			expected: `[]`,
		},
		{
			note: "undefined",
			data: `{"foo": "bar"}`,
			ref:  `data.foo.bar`,
		},
		{
			note:     "base document vars",
			data:     `{"foo": [{"bar": 0}, {"bar": 1}, {"bar": 2}]}`,
			ref:      `data.foo[x][y]`,
			expected: `[{}, {}, {}]`,
		},
		{
			note:     "input vars",
			input:    `{"foo": [{"bar": 0}, {"bar": 1}, {"bar": 2}]}`,
			ref:      `input.foo[x][y]`,
			expected: `[{}, {}, {}]`,
		},
		{
			note: "complete doc vars",
			modules: []string{
				`package test

				p = {"foo": [{"bar": 0}, {"bar": 1}, {"bar": 2}]}
				`,
			},
			ref: `data.test.p[x][y]`,
		},
		{
			note: "partial set vars",
			modules: []string{
				`package test

				p[[x,y]] { x = 0; y = 0 }
				p[[x,y]] { x = 0; y = 1 }
				p[[x,y]] { x = 1; y = 0 }
				p[[x,y]] { x = 1; y = 1 }
				`,
			},
			ref: `data.test.p[[x,y]]`, // x = z
		},
		{
			note: "partial object vars",
			modules: []string{
				`package test

				p[x] = y {
					q[x] = y
				}

				q["foo"] = {"bar": [1,2]}
				q["bar"] = {"baz": [2,3]}
				q["baz"] = {"qux": [3,4]}`,
			},
			ref: `data.test.p[x][y]`,
		},
		{
			note: "repeated",
			data: `{"arr": [{"k1": [0,0]}, {"k2": [0,0]}, {"k3": "deadbeef"}]}`,
			ref:  `data.arr[i][j][i]`,
		},
		{
			note: "full extent",
			data: `{"a": {"b": {"c": {"d": 1000}}}}`,
			modules: []string{
				`package a.b.c

				p = true`,
			},
			ref: `data.a.b`,
		},
		{
			note: "virtual document vars",
			data: `{"z": 100, "a": [{"foo": "bar"}]}`,
			modules: []string{
				`package a.b.c

				p = 1`,
				`package a.d

				q = 2
				r = 3`,
			},
			ref:      `data.a[x][y]`,
			expected: `[{}, {}, {}, {}]`, // a[0].foo, a.b.c, a.d.q, a.d.r
		},
	}

	ctx := context.Background()

	for _, tc := range tests {
		test.Subtest(t, tc.note, func(t *testing.T) {
			store := newTestStore(tc.data)
			compiler := newTestCompiler(tc.modules)
			err := storage.Txn(ctx, store, storage.TransactionParams{}, func(txn storage.Transaction) error {
				top := New(ctx, nil, compiler, store, txn)
				if tc.input != "" {
					top = top.WithInput(ast.MustParseTerm(tc.input).Value)
				}
				ref := ast.MustParseRef(tc.ref)
				err := evalDot(top, ref, func(top *Topdown) error {
					// fmt.Println("iter:", top.Vars(), top.refs, top.locals)
					return nil
				})
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newTestStore(s string) (store storage.Store) {
	if s != "" {
		store = inmem.NewFromObject(util.MustUnmarshalJSON([]byte(s)).(map[string]interface{}))
	} else {
		store = inmem.New()
	}
	return store
}

func newTestCompiler(modules []string) (compiler *ast.Compiler) {
	compiler = ast.NewCompiler()
	mods := map[string]*ast.Module{}
	for i := range modules {
		mods[fmt.Sprintf("module_%d", i)] = ast.MustParseModule(modules[i])
	}
	compiler.Compile(mods)
	if compiler.Failed() {
		panic(compiler.Errors)
	}
	return compiler
}
