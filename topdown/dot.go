// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package topdown

import (
	"fmt"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/topdown/unify"
)

func evalDot(t *Topdown, ref ast.Ref, iter Iterator) error {

	if t.Binding(ref) != nil {
		return iter(t)
	}

	return evalDotRec(t, ref, 1, iter)
}

func evalDotRec(t *Topdown, ref ast.Ref, pos int, iter Iterator) error {

	if len(ref) == pos {
		ref = unifierPlugRef(t, ref)
		return evalDotInner(t, ref, iter)
	}

	switch head := ref[pos].Value.(type) {
	case ast.Ref:
		return evalDot(t, head, func(t *Topdown) error {
			var undo *Undo
			if b := t.Binding(head); b == nil {
				var value ast.Value
				plugged := PlugValue(head, t.Binding)
				if ref, ok := plugged.(ast.Ref); ok {
					var err error
					value, err = lookupValue(t, ref)
					if err != nil {
						return err
					}
				} else {
					value = plugged
				}
				undo = t.Bind(head, value, t.bindings, nil)
			}
			err := evalDotRec(t, ref, pos+1, iter)
			if undo != nil {
				t.Unbind(undo)
			}
			return err
		})
	case ast.Array, *ast.Object, *ast.Set:
		return evalTermsRec(t, func(t *Topdown) error {
			return evalDotRec(t, ref, pos+1, iter)
		}, []*ast.Term{ref[pos]})
	default:
		return evalDotRec(t, ref, pos+1, iter)
	}
}

func evalDotInner(t *Topdown, ref ast.Ref, iter Iterator) error {

	if ref[0].Equal(ast.DefaultRootDocument) {
		e := evalDotTree{
			t:      t,
			iter:   iter,
			ref:    ref,
			pos:    1,
			parent: t.Compiler.RuleTree.Child(ref[0].Value),
		}
		return e.eval()
	}

	var term *ast.Term

	if ref[0].Equal(ast.InputRootDocument) {
		term = ast.NewTerm(t.Input)
	} else {
		term = ast.NewTerm(t.Binding(ref[0].Value))
	}

	e := evalDotTerm{
		t:     t,
		iter:  iter,
		ref:   ref,
		pos:   1,
		term:  term,
		other: t.bindings,
	}

	return e.eval()
}

type evalDotTree struct {
	t      *Topdown
	iter   Iterator
	parent *ast.TreeNode
	pos    int
	ref    ast.Ref
}

func (e evalDotTree) eval() error {

	if len(e.ref) == e.pos {
		result, err := e.evalExtent()
		if err != nil {
			if storage.IsNotFound(err) {
				return nil
			}
			return err
		} else if result != nil {
			return Continue(e.t, e.ref, result, e.t.bindings, e.iter)
		}
		return e.iter(e.t)
	}

	op := e.ref[e.pos]

	if !op.IsGround() {
		op = e.t.bindings.Plug(op)
	}

	if !op.IsGround() {
		return e.enumerate()
	}

	return e.next(op)
}

func (e evalDotTree) next(op *ast.Term) error {

	var child *ast.TreeNode

	if e.parent != nil {
		child = e.parent.Child(op.Value)
		if child != nil && len(child.Values) > 0 {
			r := evalDotRules{
				t:    e.t,
				iter: e.iter,
				ref:  e.ref,
				pos:  e.pos,
			}
			return r.eval()
		}
	}

	cpy := e
	cpy.parent = child
	cpy.pos++

	return cpy.eval()
}

func (e evalDotTree) enumerate() error {

	prefix := unifierPlugRef(e.t, e.ref[:e.pos])

	doc, err := e.t.Resolve(prefix)
	if err != nil {
		if !storage.IsNotFound(err) {
			return err
		}
	}

	if err == nil {
		switch doc := doc.(type) {
		case map[string]interface{}:
			for k := range doc {
				key := ast.StringTerm(k)
				undo := unify.Unify(key, e.t.bindings, e.ref[e.pos], e.t.bindings)
				err := e.next(key)
				unify.UndoAll(undo)
				if err != nil {
					return err
				}
			}
		case []interface{}:
			for i := range doc {
				key := ast.IntNumberTerm(i)
				undo := unify.Unify(key, e.t.bindings, e.ref[e.pos], e.t.bindings)
				err := e.next(key)
				unify.UndoAll(undo)
				if err != nil {
					return err
				}
			}
		}
	}

	if e.parent == nil {
		return nil
	}

	for k := range e.parent.Children {
		key := ast.NewTerm(k)
		undo := unify.Unify(key, e.t.bindings, e.ref[e.pos], e.t.bindings)
		err := e.next(key)
		unify.UndoAll(undo)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e evalDotTree) evalExtent() (ast.Value, error) {

	plugged := unifierPlugRef(e.t, e.ref)

	base, readErr := e.t.Resolve(plugged)
	if readErr != nil {
		if !storage.IsNotFound(readErr) {
			return nil, readErr
		}
	}

	var virtual ast.Object

	if e.parent != nil {
		var err error
		virtual, err = e.evalRecursive(unifierPlugRef(e.t, e.ref), e.parent)
		if err != nil {
			return nil, err
		}
	}

	if virtual == nil {
		return nil, readErr
	}

	if readErr == nil {
		baseValue, err := ast.InterfaceToValue(base)
		if err != nil {
			return nil, err
		}
		virtual, _ = baseValue.(ast.Object).Merge(virtual)
	}

	return virtual, nil
}

func (e evalDotTree) evalRecursive(path ast.Ref, node *ast.TreeNode) (ast.Object, error) {

	result := ast.Object{}

	for _, child := range node.Children {
		if child.Hide {
			continue
		}

		path = append(path, ast.NewTerm(child.Key))

		var save ast.Value
		var err error

		if len(child.Values) > 0 {
			e := evalDotRules{
				t: e.t,
				iter: func(t *Topdown) error {
					save = t.Binding(path)
					return nil
				},
				ref: path,
				pos: len(path) - 1,
			}
			err = e.eval()
		} else {
			save, err = e.evalRecursive(path, child)
		}

		if err != nil {
			return nil, err
		}

		if save != nil {
			result, _ = result.Merge(ast.Object{ast.Item(path[len(path)-1], ast.NewTerm(save))})
		}

		path = path[:len(path)-1]
	}

	return result, nil
}

type evalDotRules struct {
	t    *Topdown
	iter Iterator
	pos  int
	ref  ast.Ref
}

func (e evalDotRules) eval() error {

	prefix := unifierPlugRef(e.t, e.ref[:e.pos+1])

	index := e.t.Compiler.RuleIndex(prefix)
	ir, err := index.Lookup(valueResolver{e.t})
	if err != nil {
		return err
	}

	switch ir.Kind {
	case ast.PartialSetDoc:
		r := evalDotPartialDoc{
			t:      e.t,
			iter:   e.iter,
			pos:    e.pos,
			ref:    e.ref,
			ir:     ir,
			empty:  &ast.Set{},
			reduce: setReduce,
			deref:  setDeref,
		}
		return r.eval()
	case ast.PartialObjectDoc:
		r := evalDotPartialDoc{
			t:      e.t,
			iter:   e.iter,
			pos:    e.pos,
			ref:    e.ref,
			ir:     ir,
			empty:  ast.Object{},
			reduce: objectReduce,
			deref:  objectDeref,
		}
		return r.eval()
	default:
		r := evalDotCompleteDoc{
			t:    e.t,
			iter: e.iter,
			pos:  e.pos,
			ref:  e.ref,
			ir:   ir,
		}
		return r.eval()
	}
}

type evalDotCompleteDoc struct {
	t    *Topdown
	iter Iterator
	pos  int
	ref  ast.Ref
	ir   *ast.IndexResult
}

func (e evalDotCompleteDoc) eval() error {

	if e.ir.Empty() {
		return nil
	}

	if len(e.ir.Rules) > 0 && len(e.ir.Rules[0].Head.Args) > 0 {
		return nil
	}

	var result ast.Value

	for i := range e.ir.Rules {
		var err error
		if result, err = e.evalRule(result, e.ir.Rules[i]); err != nil {
			return err
		}
		if len(e.ir.Else) > 0 {
			return fmt.Errorf("not implemented: else")
		}
	}

	if e.ir.Default != nil {
		return fmt.Errorf("not implemented: default")
	}

	return nil
}

func (e evalDotCompleteDoc) evalRule(prev ast.Value, rule *ast.Rule) (ast.Value, error) {

	child := e.t.Child(rule.Body)

	result := prev // TODO(tsandall) conflicts
	if prev != nil {
		return nil, fmt.Errorf("not implemented: conflicts")
	}

	err := eval(child, func(child *Topdown) error {
		r := evalDotTerm{
			t:     e.t,
			iter:  e.iter,
			ref:   e.ref,
			term:  rule.Head.Value,
			pos:   e.pos + 1,
			other: child.bindings,
			bind:  true,
		}
		return r.eval()
	})

	return result, err
}

type evalDotPartialDoc struct {
	t      *Topdown
	iter   Iterator
	pos    int
	ref    ast.Ref
	ir     *ast.IndexResult
	empty  ast.Value
	reduce func(*Topdown, *ast.Rule, ast.Value) (ast.Value, error)
	deref  func(*ast.Rule) *ast.Term
}

func (e evalDotPartialDoc) eval() error {

	if e.ir.Empty() {
		return nil
	}

	if len(e.ref) == (e.pos + 1) {
		return e.evalAllRules(e.ir.Rules)
	}

	for _, rule := range e.ir.Rules {
		if err := e.evalOneRule(rule); err != nil {
			return err
		}
	}

	return nil
}

func (e evalDotPartialDoc) evalAllRules(rules []*ast.Rule) error {
	result := e.empty
	for i := range rules {
		child := e.t.Child(rules[i].Body)
		err := eval(child, func(child *Topdown) error {
			var err error
			result, err = e.reduce(child, rules[i], result)
			return err
		})
		if err != nil {
			return err
		}
	}
	return Continue(e.t, e.ref, result, e.t.bindings, e.iter)
}

func (e evalDotPartialDoc) evalOneRule(rule *ast.Rule) error {
	key := e.ref[e.pos+1]
	child := e.t.Child(rule.Body)
	undo := unify.Unify(key, e.t.bindings, rule.Head.Key, child.bindings)
	err := eval(child, func(child *Topdown) error {
		r := evalDotTerm{
			t:     e.t,
			iter:  e.iter,
			ref:   e.ref,
			term:  e.deref(rule),
			pos:   e.pos + 2,
			other: child.bindings,
			bind:  true,
		}
		return r.eval()
	})
	unify.UndoAll(undo)
	return err
}

type evalDotTerm struct {
	t     *Topdown
	iter  Iterator
	ref   ast.Ref
	pos   int
	term  *ast.Term
	other *unify.Unifier
	bind  bool
}

func (e evalDotTerm) eval() error {

	if e.pos == len(e.ref) {
		if e.bind {
			return Continue(e.t, e.ref, e.term.Value, e.other, e.iter)
		}
		return e.iter(e.t)
	}

	op := e.ref[e.pos]
	plugged := e.t.bindings.Plug(op)

	if !plugged.IsGround() {
		return e.enumerate()
	}

	child := e.get(plugged)
	if child == nil {
		return nil
	}

	return e.next(child)
}

func (e evalDotTerm) enumerate() error {

	switch v := e.term.Value.(type) {
	case *ast.Set:
		for _, elem := range *v {
			undo := unify.Unify(elem, e.other, e.ref[e.pos], e.t.bindings)
			err := e.next(elem)
			unify.UndoAll(undo)
			if err != nil {
				return err
			}
		}
	case ast.Array:
		for i := range v {
			pos := ast.IntNumberTerm(i)
			undo := unify.Unify(pos, e.other, e.ref[e.pos], e.t.bindings)
			err := e.next(v[i])
			unify.UndoAll(undo)
			if err != nil {
				return err
			}
		}
	case ast.Object:
		for _, pair := range v {
			undo := unify.Unify(pair[0], e.other, e.ref[e.pos], e.t.bindings)
			err := e.next(pair[1])
			unify.UndoAll(undo)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (e evalDotTerm) get(k *ast.Term) *ast.Term {
	switch v := e.term.Value.(type) {
	case *ast.Set:
		if v.Contains(k) {
			return k
		}
	case ast.Array:
		return v.Get(k)
	case ast.Object:
		return v.Get(k)
	}
	return nil
}

func (e evalDotTerm) next(child *ast.Term) error {
	cpy := e
	cpy.term = child
	cpy.pos++
	return cpy.eval()
}

func unifierPlugRef(t *Topdown, ref ast.Ref) ast.Ref {
	cpy := make(ast.Ref, len(ref))
	cpy[0] = ref[0]
	for i := 1; i < len(ref); i++ {
		cpy[i] = PlugTerm(ref[i], t.Binding)
	}
	return cpy
}

func setReduce(t *Topdown, rule *ast.Rule, acc ast.Value) (ast.Value, error) {
	val := PlugTerm(rule.Head.Key, t.Binding)
	var set *ast.Set
	if acc == nil {
		set = &ast.Set{}
	} else {
		set = acc.(*ast.Set)
	}
	set.Add(val)
	return set, nil
}

func setDeref(rule *ast.Rule) *ast.Term {
	return rule.Head.Key
}

func objectReduce(t *Topdown, rule *ast.Rule, acc ast.Value) (ast.Value, error) {
	key := PlugTerm(rule.Head.Key, t.Binding)
	val := PlugTerm(rule.Head.Value, t.Binding)
	var obj ast.Object
	if acc == nil {
		obj = ast.Object{}
	} else {
		obj = acc.(ast.Object)
	}
	if exist := obj.Get(key); exist != nil && !exist.Equal(val) {
		return nil, objectDocKeyConflictErr(t.currentLocation(rule))
	}
	obj = append(obj, ast.Item(key, val))
	return obj, nil
}

func objectDeref(rule *ast.Rule) *ast.Term {
	return rule.Head.Value
}
