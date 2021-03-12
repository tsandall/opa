// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package inmem implements an in-memory version of the policy engine's storage
// layer.
//
// The in-memory store is used as the default storage layer implementation. The
// in-memory store supports multi-reader/single-writer concurrency with
// rollback.
//
// Callers should assume the in-memory store does not make copies of written
// data. Once data is written to the in-memory store, it should not be modified
// (outside of calling Store.Write). Furthermore, data read from the in-memory
// store should be treated as read-only.
package inmem

import (
	"container/list"
	"context"
	"fmt"
	"io"

	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/storage/internal/pluggable"
	"github.com/open-policy-agent/opa/util"
)

// New returns an empty in-memory store.
func New() storage.Store {
	i := &inmem{
		data:     map[string]interface{}{},
		policies: map[string][]byte{},
	}
	return pluggable.New(func(ctx context.Context, xid uint64, write bool, context *storage.Context) (pluggable.Transaction, error) {
		txn := &transaction{
			xid:      xid,
			write:    write,
			db:       i,
			policies: map[string]policyUpdate{},
			updates:  list.New(),
			context:  context,
		}
		return txn, nil
	})
}

// NewFromObject returns a new in-memory store from the supplied data object.
func NewFromObject(data map[string]interface{}) storage.Store {
	db := New()
	ctx := context.Background()
	txn, err := db.NewTransaction(ctx, storage.WriteParams)
	if err != nil {
		panic(err)
	}
	if err := db.Write(ctx, txn, storage.AddOp, storage.Path{}, data); err != nil {
		panic(err)
	}
	if err := db.Commit(ctx, txn); err != nil {
		panic(err)
	}
	return db
}

// NewFromReader returns a new in-memory store from a reader that produces a
// JSON serialized object. This function is for test purposes.
func NewFromReader(r io.Reader) storage.Store {
	d := util.NewJSONDecoder(r)
	var data map[string]interface{}
	if err := d.Decode(&data); err != nil {
		panic(err)
	}
	return NewFromObject(data)
}

type inmem struct {
	data     map[string]interface{} // raw data
	policies map[string][]byte      // raw policies
}

var doesNotExistMsg = "document does not exist"
var rootMustBeObjectMsg = "root must be object"
var rootCannotBeRemovedMsg = "root cannot be removed"
var outOfRangeMsg = "array index out of range"
var arrayIndexTypeMsg = "array index must be integer"

func invalidPatchError(f string, a ...interface{}) *storage.Error {
	return &storage.Error{
		Code:    storage.InvalidPatchErr,
		Message: fmt.Sprintf(f, a...),
	}
}

func notFoundError(path storage.Path) *storage.Error {
	return notFoundErrorHint(path, doesNotExistMsg)
}

func notFoundErrorHint(path storage.Path, hint string) *storage.Error {
	return notFoundErrorf("%v: %v", path.String(), hint)
}

func notFoundErrorf(f string, a ...interface{}) *storage.Error {
	msg := fmt.Sprintf(f, a...)
	return &storage.Error{
		Code:    storage.NotFoundErr,
		Message: msg,
	}
}
