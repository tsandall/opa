// Copyright 2021 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package pluggable

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/util"
)

type store struct {
	rmu      sync.RWMutex                      // reader-writer lock
	wmu      sync.Mutex                        // writer lock
	xid      uint64                            // last generated transaction id
	triggers map[*handle]storage.TriggerConfig // registered triggers
	factory  func(context.Context, uint64, bool, *storage.Context) (Transaction, error)
}

type Transaction interface {
	storage.Transaction

	ListPolicies(context.Context) ([]string, error)
	GetPolicy(context.Context, string) ([]byte, error)
	UpsertPolicy(context.Context, string, []byte) error
	DeletePolicy(context.Context, string) error
	Read(ctx context.Context, path storage.Path) (interface{}, error)
	Write(ctx context.Context, op storage.PatchOp, path storage.Path, value interface{}) error
	Commit(ctx context.Context) (storage.TriggerEvent, error)
	Abort(ctx context.Context)
}

type wrapper struct {
	write      bool
	stale      bool
	db         *store
	underlying Transaction
}

func (w *wrapper) ID() uint64 {
	return w.underlying.ID()
}

func New(factory func(context.Context, uint64, bool, *storage.Context) (Transaction, error)) storage.Store {
	return &store{
		factory:  factory,
		triggers: map[*handle]storage.TriggerConfig{},
	}
}

func (db *store) NewTransaction(ctx context.Context, params ...storage.TransactionParams) (storage.Transaction, error) {
	var write bool
	var context *storage.Context
	if len(params) > 0 {
		write = params[0].Write
		context = params[0].Context
	}
	xid := atomic.AddUint64(&db.xid, uint64(1))
	if write {
		db.wmu.Lock()
	} else {
		db.rmu.RLock()
	}
	underlying, err := db.factory(ctx, xid, write, context)
	if err != nil {
		if write {
			db.wmu.Unlock()
		} else {
			db.rmu.RUnlock()
		}
		return nil, err
	}
	txn := &wrapper{
		write:      write,
		db:         db,
		underlying: underlying,
	}
	return txn, err
}

func (db *store) Commit(ctx context.Context, txn storage.Transaction) error {
	w, err := db.unwrap(txn)
	if err != nil {
		return err
	}
	if w.write {
		db.rmu.Lock()
		event, err := w.underlying.Commit(ctx)
		if err == nil {
			db.runOnCommitTriggers(ctx, txn, event)
		}
		// Mark the transaction stale after executing triggers so they can
		// perform store operations if needed.
		w.stale = true
		db.rmu.Unlock()
		db.wmu.Unlock()
	} else {
		db.rmu.RUnlock()
	}
	return nil
}

func (db *store) Abort(ctx context.Context, txn storage.Transaction) {
	w, err := db.unwrap(txn)
	if err != nil {
		panic(err)
	}
	w.stale = true
	if w.write {
		db.wmu.Unlock()
	} else {
		db.rmu.RUnlock()
	}
}

func (db *store) ListPolicies(ctx context.Context, txn storage.Transaction) ([]string, error) {
	w, err := db.unwrap(txn)
	if err != nil {
		return nil, err
	}
	return w.underlying.ListPolicies(ctx)
}

func (db *store) GetPolicy(ctx context.Context, txn storage.Transaction, id string) ([]byte, error) {
	w, err := db.unwrap(txn)
	if err != nil {
		return nil, err
	}
	return w.underlying.GetPolicy(ctx, id)
}

func (db *store) UpsertPolicy(ctx context.Context, txn storage.Transaction, id string, bs []byte) error {
	w, err := db.unwrap(txn)
	if err != nil {
		return err
	}
	return w.underlying.UpsertPolicy(ctx, id, bs)
}

func (db *store) DeletePolicy(ctx context.Context, txn storage.Transaction, id string) error {
	w, err := db.unwrap(txn)
	if err != nil {
		return err
	}
	if _, err := w.underlying.GetPolicy(ctx, id); err != nil {
		return err
	}
	return w.underlying.DeletePolicy(ctx, id)
}

func (db *store) Register(ctx context.Context, txn storage.Transaction, config storage.TriggerConfig) (storage.TriggerHandle, error) {
	w, err := db.unwrap(txn)
	if err != nil {
		return nil, err
	}
	if !w.write {
		return nil, &storage.Error{
			Code:    storage.InvalidTransactionErr,
			Message: "triggers must be registered with a write transaction",
		}
	}
	h := &handle{db}
	db.triggers[h] = config
	return h, nil
}

func (db *store) Read(ctx context.Context, txn storage.Transaction, path storage.Path) (interface{}, error) {
	w, err := db.unwrap(txn)
	if err != nil {
		return nil, err
	}
	return w.underlying.Read(ctx, path)
}

func (db *store) Write(ctx context.Context, txn storage.Transaction, op storage.PatchOp, path storage.Path, value interface{}) error {
	w, err := db.unwrap(txn)
	if err != nil {
		return err
	}
	val := util.Reference(value)
	if err := util.RoundTrip(val); err != nil {
		return err
	}
	return w.underlying.Write(ctx, op, path, *val)
}

func (db *store) runOnCommitTriggers(ctx context.Context, txn storage.Transaction, event storage.TriggerEvent) {
	for _, t := range db.triggers {
		t.OnCommit(ctx, txn, event)
	}
}

func (db *store) unwrap(txn storage.Transaction) (*wrapper, error) {
	w, ok := txn.(*wrapper)
	if !ok {
		return nil, &storage.Error{
			Code:    storage.InvalidTransactionErr,
			Message: fmt.Sprintf("unexpected transaction type %T", txn),
		}
	}
	if w.db != db {
		return nil, &storage.Error{
			Code:    storage.InvalidTransactionErr,
			Message: "unknown transaction",
		}
	}
	if w.stale {
		return nil, &storage.Error{
			Code:    storage.InvalidTransactionErr,
			Message: "stale transaction",
		}
	}
	return w, nil
}

type handle struct {
	db *store
}

func (h *handle) Unregister(ctx context.Context, txn storage.Transaction) {
	w, err := h.db.unwrap(txn)
	if err != nil {
		panic(err)
	}
	if !w.write {
		panic(&storage.Error{
			Code:    storage.InvalidTransactionErr,
			Message: "triggers must be unregistered with a write transaction",
		})
	}
	delete(h.db.triggers, h)
}
