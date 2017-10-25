// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package unify

import (
	"fmt"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/util"
)

// Unify performs unification on a and b recursively. If a and b can be
// unified, the return value is a non-nil value that can be used to undo
// changes to the unifiers.
func Unify(a *ast.Term, u1 *Unifier, b *ast.Term, u2 *Unifier) Undo {
	return unify(a, u1, b, u2)
}

// Undo defines the interface for undoing a change to a unifier.
type Undo interface {
	Undo()
	Chain(undo Undo) Undo
}

func UndoAll(undo Undo) {
	if undo != nil {
		undo.Undo()
	}
}

// Unifier contains bindings produced by unification.
type Unifier struct {
	values *util.HashMap
}

// New returns an empty Unifier object with no bindings.
func New() *Unifier {

	eq := func(a, b util.T) bool {
		v1, ok1 := a.(*ast.Term)
		if ok1 {
			v2 := b.(*ast.Term)
			return v1.Equal(v2)
		}
		uv1 := a.(*value)
		uv2 := b.(*value)
		return uv1.equal(uv2)
	}

	hash := func(x util.T) int {
		v := x.(*ast.Term)
		return v.Hash()
	}

	values := util.NewHashMap(eq, hash)

	return &Unifier{values}
}

func (u *Unifier) Bindings() [][2]*ast.Term {
	result := make([][2]*ast.Term, 0, u.values.Len())
	u.values.Iter(func(k, _ util.T) bool {
		term := k.(*ast.Term)
		value := u.Plug(term)
		result = append(result, [2]*ast.Term{term, value})
		return false
	})
	return result
}

// Plug substitutes variables in a with bindings in u.
func (u *Unifier) Plug(a *ast.Term) *ast.Term {
	switch v := a.Value.(type) {
	case ast.Var:
		b, next := u.apply(a)
		if a != b || u != next {
			return next.Plug(b)
		}
		return b
	case ast.Array:
		cpy := *a
		arr := make(ast.Array, len(v))
		for i := 0; i < len(arr); i++ {
			arr[i] = u.Plug(v[i])
		}
		cpy.Value = arr
		return &cpy
	case ast.Object:
		cpy := *a
		obj := make(ast.Object, len(v))
		for i := 0; i < len(obj); i++ {
			obj[i] = ast.Item(u.Plug(v[i][0]), u.Plug(v[i][1]))
		}
		cpy.Value = obj
		return &cpy
	case *ast.Set:
		cpy := *a
		cpy.Value, _ = v.Map(func(x *ast.Term) (*ast.Term, error) {
			return u.Plug(x), nil
		})
		return &cpy
	}
	return a
}

func (u *Unifier) String() string {
	if u == nil {
		return "{}"
	}
	return u.values.String()
}

func (u *Unifier) bind(a *ast.Term, b *ast.Term, other *Unifier) Undo {
	u.values.Put(a, value{
		u: other,
		v: b,
	})
	return &undo{a, u, nil}
}

func (u *Unifier) apply(a *ast.Term) (*ast.Term, *Unifier) {
	val, ok := u.get(a)
	if !ok {
		return a, u
	}
	return val.u.apply(val.v)
}

func (u *Unifier) delete(v *ast.Term) {
	u.values.Delete(v)
}

func (u *Unifier) get(v *ast.Term) (value, bool) {
	if u == nil {
		return value{}, false
	}
	r, ok := u.values.Get(v)
	if !ok {
		return value{}, false
	}
	return r.(value), true
}

type undo struct {
	k    *ast.Term
	u    *Unifier
	next Undo
}

func (u *undo) Undo() {
	if u.u == nil {
		return
	}
	u.u.delete(u.k)
	if u.next != nil {
		u.next.Undo()
	}
}

func (u *undo) Chain(other Undo) Undo {
	u.next = other
	return u
}

type value struct {
	u *Unifier
	v *ast.Term
}

func (v value) String() string {
	return fmt.Sprintf("<%v, %p>", v.v, v.u)
}

func (v value) equal(other *value) bool {
	if v.u == other.u {
		return v.v.Equal(other.v)
	}
	return false
}

func unify(a *ast.Term, u1 *Unifier, b *ast.Term, u2 *Unifier) Undo {
	a, u1 = u1.apply(a)
	b, u2 = u2.apply(b)
	switch vA := a.Value.(type) {
	case ast.Var:
		return unifyValues(a, u1, b, u2)
	case ast.Null:
		switch b.Value.(type) {
		case ast.Var, ast.Null:
			return unifyValues(a, u1, b, u2)
		}
	case ast.Boolean:
		switch b.Value.(type) {
		case ast.Var, ast.Boolean:
			return unifyValues(a, u1, b, u2)
		}
	case ast.Number:
		switch b.Value.(type) {
		case ast.Var, ast.Number:
			return unifyValues(a, u1, b, u2)
		}
	case ast.String:
		switch b.Value.(type) {
		case ast.Var, ast.String:
			return unifyValues(a, u1, b, u2)
		}
	case ast.Array:
		switch vB := b.Value.(type) {
		case ast.Var:
			return unifyValues(a, u1, b, u2)
		case ast.Array:
			if len(vA) != len(vB) {
				return nil
			}
			var prev Undo
			for i := 0; i < len(vA); i++ {
				if undo := Unify(vA[i], u1, vB[i], u2); undo != nil {
					prev = undo.Chain(prev)
				} else {
					UndoAll(prev)
					return nil
				}
			}
			return prev
		}
	case ast.Object:
		switch vB := b.Value.(type) {
		case ast.Var:
			return unifyValues(a, u1, b, u2)
		case ast.Object:
			if len(vA) != len(vB) {
				return nil
			}
			var prev Undo
			for i := range vA {
				keyA, valueA := vA[i][0], vA[i][1]
				if valueB := vB.Get(keyA); valueB != nil {
					if undo := Unify(valueA, u1, valueB, u2); undo != nil {
						prev = undo.Chain(prev)
					} else {
						UndoAll(prev)
						return nil
					}
				} else {
					UndoAll(prev)
					return nil
				}
			}
			return prev
		}
	case *ast.Set:
		switch vB := b.Value.(type) {
		case ast.Var:
			return unifyValues(a, u1, b, u2)
		case *ast.Set:
			if vA.Equal(vB) {
				return &undo{}
			}
		}
	}
	return nil
}

func unifyValues(a *ast.Term, u1 *Unifier, b *ast.Term, u2 *Unifier) Undo {
	a, u1 = u1.apply(a)
	b, u2 = u2.apply(b)
	_, varA := a.Value.(ast.Var)
	_, varB := b.Value.(ast.Var)
	if varA && varB {
		if u1 == u2 && a.Equal(b) {
			return &undo{}
		}
		return u1.bind(a, b, u2)
	} else if varA && !varB {
		return u1.bind(a, b, u2)
	} else if varB && !varA {
		return u2.bind(b, a, u1)
	} else if a.Equal(b) {
		return &undo{}
	}
	return nil
}
