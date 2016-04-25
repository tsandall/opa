// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package eval

import (
	"fmt"
	"strings"

	"github.com/open-policy-agent/opa/opalog"
)

// Indices is the the top-level structure supporting exact match, reverse lookup
// of values in storage referred to by non-ground references (e.g., "a.b[i].c[j]").
// The indices is modelled as a collection of indexes where each index is built for
// a particular reference encountered in a query.
type Indices struct {
	table map[int]*indicesNode
}

type indicesNode struct {
	key   opalog.Ref
	value *Index
	next  *indicesNode
}

// NewIndices returns a new, empty indices.
func NewIndices() *Indices {
	return &Indices{
		table: map[int]*indicesNode{},
	}
}

// Build updates the indices to include an index for the reference.
func (ind *Indices) Build(store *Storage, ref opalog.Ref) error {
	index := NewIndex()
	err := walkRef(store, ref, func(bindings *hashMap, v interface{}) {
		index.Add(v, bindings)
	})
	if err != nil {
		return err
	}
	hashCode := ref.Hash()
	head := ind.table[hashCode]
	entry := &indicesNode{
		key:   ref,
		value: index,
		next:  head,
	}
	ind.table[hashCode] = entry
	return nil
}

// Contains returns true if an index has been built for the reference.
func (ind *Indices) Contains(ref opalog.Ref) bool {
	hashCode := ref.Hash()
	for entry := ind.table[hashCode]; entry != nil; entry = entry.next {
		if entry.key.Equal(ref) {
			return true
		}
	}
	return false
}

// Get returns the index for a reference's value.
func (ind *Indices) Get(ref opalog.Ref) *Index {
	hashCode := ref.Hash()
	for entry := ind.table[hashCode]; entry != nil; entry = entry.next {
		if entry.key.Equal(ref) {
			return entry.value
		}
	}
	return nil
}

// Add updates the index for a particular reference by adding the bindings
// associated with a value.
func (ind *Indices) Add(ref opalog.Ref, val interface{}, bindings *hashMap) {
	hashCode := ref.Hash()
	for entry := ind.table[hashCode]; entry != nil; entry = entry.next {
		if entry.key.Equal(ref) {
			entry.value.Add(val, bindings)
		}
	}
}

// Remove updates the index for a particular reference by removing the bindings
// associated with a value.
func (ind *Indices) Remove(ref opalog.Ref, val interface{}, bindings *hashMap) {

}

// Replace updates the indices so that the index pointed by the oldValue is now
// pointed to by the newValue.
func (ind *Indices) Replace(ref opalog.Ref, oldValue interface{}, newValue interface{}) {

}

func (ind *Indices) String() string {
	buf := []string{}
	for _, head := range ind.table {
		for entry := head; entry != nil; entry = entry.next {
			str := fmt.Sprintf("%v: %s", entry.key, entry.value)
			buf = append(buf, str)
		}
	}
	return "{" + strings.Join(buf, ", ") + "}"
}

// Index is a structure that provides reverse lookups on values referred to
// by non-ground references in queries.
type Index struct {
	table map[int]*indexNode
}

type indexNode struct {
	key   interface{}
	value bindingSet
	next  *indexNode
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		table: map[int]*indexNode{},
	}
}

// Add updates the index for the value to include the supplied bindings.
func (ind *Index) Add(val interface{}, bindings *hashMap) {

	hashCode := hash(val)

	for entry := ind.table[hashCode]; entry != nil; entry = entry.next {
		if Compare(entry.key, val) == 0 {
			entry.value.Add(bindings)
			return
		}
	}

	head := ind.table[hashCode]

	bindingsSet := newBindingSet()
	bindingsSet.Add(bindings)

	entry := &indexNode{
		key:   val,
		value: bindingsSet,
		next:  head,
	}

	ind.table[hashCode] = entry
}

// Iter invokes the iter function for each binding associated with the value.
func (ind *Index) Iter(val interface{}, iter func(*hashMap) error) error {
	hashCode := hash(val)
	head := ind.table[hashCode]
	for entry := head; entry != nil; entry = entry.next {
		if Compare(entry.key, val) == 0 {
			return entry.value.Iter(iter)
		}
	}
	return nil
}

func (ind *Index) String() string {

	buf := []string{}

	for _, head := range ind.table {
		for entry := head; entry != nil; entry = entry.next {
			str := fmt.Sprintf("%v: %v", entry.key, entry.value)
			buf = append(buf, str)
		}
	}

	return "{" + strings.Join(buf, ", ") + "}"
}

func hash(v interface{}) int {
	switch v := v.(type) {
	case []interface{}:
		var h int
		for _, e := range v {
			h += hash(e)
		}
		return h
	case map[string]interface{}:
		var h int
		for k, v := range v {
			h += hash(k) + hash(v)
		}
		return h
	case string:
		// TOOD(tsandall) move to utils package
		// FNV-1a hashing
		var h uint32
		for i := 0; i < len(v); i++ {
			h ^= uint32(v[i])
			h *= 16777619
		}
		return int(h)
	case bool:
		if v {
			return 1
		}
		return 0
	case nil:
		return 0
	case float64:
		return int(v)
	}
	panic(fmt.Sprintf("illegal argument: %v (%T)", v, v))
}

func walkRef(store *Storage, ref opalog.Ref, iter func(*hashMap, interface{})) error {
	return walkRefRec(store, ref, opalog.EmptyRef(), newHashMap(), iter)
}

func walkRefRec(store *Storage, ref opalog.Ref, path opalog.Ref, bindings *hashMap, iter func(*hashMap, interface{})) error {

	if len(ref) == 0 {
		node, err := lookup(store, path)
		if err != nil {
			return err
		}
		iter(bindings, node)
		return nil
	}

	head := ref[0]
	tail := ref[1:]

	headVar, isVar := head.Value.(opalog.Var)

	if !isVar || len(path) == 0 {
		path = append(path, head)
		return walkRefRec(store, tail, path, bindings, iter)
	}

	// Binding does not exist, we will lookup the collection and enumerate
	// the keys below.
	node, err := lookup(store, path)
	if err != nil {
		switch err := err.(type) {
		case *StorageError:
			if err.Code == StorageNotFoundErr {
				return nil
			}
		}
		return err
	}

	switch node := node.(type) {
	case map[string]interface{}:
		for key := range node {
			path = append(path, opalog.StringTerm(key))
			cpy := bindings.Copy()
			cpy.Put(headVar, opalog.String(key))
			err := walkRefRec(store, tail, path, cpy, iter)
			if err != nil {
				return err
			}
			path = path[:len(path)-1]
		}
		return nil
	case []interface{}:
		for i := range node {
			path = append(path, opalog.NumberTerm(float64(i)))
			cpy := bindings.Copy()
			cpy.Put(headVar, opalog.Number(float64(i)))
			err := walkRefRec(store, tail, path, cpy, iter)
			if err != nil {
				return err
			}
			path = path[:len(path)-1]
		}
		return nil
	default:
		return fmt.Errorf("unexpected non-composite encountered via reference %v at path: %v", ref, path)
	}
}

type bindingSetNode struct {
	v    *hashMap
	next *bindingSetNode
}

type bindingSet map[int]*bindingSetNode

func newBindingSet() bindingSet {
	return map[int]*bindingSetNode{}
}

func (set bindingSet) Contains(v *hashMap) bool {
	hash := v.Hash()
	head := set[hash]
	for entry := head; entry != nil; entry = entry.next {
		if entry.v.Equal(v) {
			return true
		}
	}
	return false
}

func (set bindingSet) Add(v *hashMap) {
	hash := v.Hash()
	head := set[hash]
	for entry := head; entry != nil; entry = entry.next {
		if entry.v.Equal(v) {
			return
		}
	}
	set[hash] = &bindingSetNode{v, head}
}

func (set bindingSet) Iter(iter func(*hashMap) error) error {
	for _, head := range set {
		for entry := head; entry != nil; entry = entry.next {
			if err := iter(entry.v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (set bindingSet) Remove(v *hashMap) {
	hash := v.Hash()
	head := set[hash]
	var prev *bindingSetNode
	for entry := head; entry != nil; entry = entry.next {
		if entry.v.Equal(v) {
			if prev == nil {
				set[hash] = entry.next
			} else {
				prev.next = entry.next
			}
			return
		}
		prev = entry
	}
}

func (set bindingSet) String() string {
	buf := []string{}
	set.Iter(func(bindings *hashMap) error {
		buf = append(buf, bindings.String())
		return nil
	})
	return "{" + strings.Join(buf, ", ") + "}"
}
