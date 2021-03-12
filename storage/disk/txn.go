// Copyright 2021 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dgraph-io/badger"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/util"
)

type transaction struct {
	db           *disk
	xid          uint64
	context      *storage.Context
	write        bool
	underlying   *badger.Txn
	dataWrites   []storage.DataEvent
	policyWrites []storage.PolicyEvent
}

func newTransaction(xid uint64, write bool, underlying *badger.Txn, context *storage.Context, db *disk) *transaction {
	return &transaction{
		xid:        xid,
		write:      write,
		underlying: underlying,
		db:         db,
		context:    context,
	}
}

func (txn *transaction) ID() uint64 {
	return txn.xid
}

func (txn *transaction) Commit(_ context.Context) (storage.TriggerEvent, error) {
	var event storage.TriggerEvent
	event.Context = txn.context
	event.Data = txn.dataWrites
	event.Policy = txn.policyWrites
	return event, txn.underlying.Commit()
}

func (txn *transaction) Abort(_ context.Context) {
	txn.underlying.Discard()
}

func (txn *transaction) Read(_ context.Context, path storage.Path) (interface{}, error) {

	path = getPrefixedPath("data", path)

	// fmt.Println("read:", path)

	key, tail, scan, err := txn.partitionRead(path)

	// fmt.Println("  --> key:", string(key), "tail:", tail)
	// defer func() {
	// 	fmt.Println("  --> result:", result, "err:", err)
	// }()

	if err != nil {
		return nil, err
	}

	u := txn.underlying

	if scan {
		return txn.readScan(path)
	}

	item, err := u.Get(key)
	if err != nil {
		if badger.ErrKeyNotFound == err {
			return nil, errNotFound
		}
		return nil, err
	}

	var x interface{}

	err = item.Value(func(bs []byte) error {
		return util.NewJSONDecoder(bytes.NewReader(bs)).Decode(&x)
	})

	if err != nil {
		return nil, err
	}

	return ptr(x, tail)
}

func (txn *transaction) Write(_ context.Context, op storage.PatchOp, path storage.Path, value interface{}) error {

	txn.dataWrites = append(txn.dataWrites, storage.DataEvent{
		Removed: op == storage.RemoveOp,
		Path:    path,
		Data:    value,
	})

	switch op {
	case storage.AddOp:
		return txn.writeAdd(path, value)
	case storage.ReplaceOp:
		return errors.New("not implemented: write: replace")
	case storage.RemoveOp:
		return errors.New("not implemented: write: remove")
	default:
		return errInvalidPatch
	}
}

func (txn *transaction) ListPolicies(_ context.Context) ([]string, error) {

	var result []string

	it := txn.underlying.NewIterator(badger.IteratorOptions{
		Prefix: []byte("/policies/"),
	})

	defer it.Close()

	var key []byte

	for it.Rewind(); it.Valid(); it.Next() {
		key = it.Item().KeyCopy(key)
		result = append(result, string(key[len("/policies/"):]))
	}

	return result, nil
}

func (txn *transaction) GetPolicy(_ context.Context, id string) ([]byte, error) {
	// TODO(tsandall): need to encode slashes in id?
	key := []byte(fmt.Sprintf("/policies/%v", id))
	item, err := txn.underlying.Get(key)
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

func (txn *transaction) UpsertPolicy(_ context.Context, id string, bs []byte) error {
	txn.policyWrites = append(txn.policyWrites, storage.PolicyEvent{
		ID:   id,
		Data: bs,
	})
	// TODO(tsandall): need to encode slashes in id?
	key := []byte(fmt.Sprintf("/policies/%v", id))
	return txn.underlying.Set(key, bs)
}

func (txn *transaction) DeletePolicy(_ context.Context, id string) error {
	txn.policyWrites = append(txn.policyWrites, storage.PolicyEvent{
		ID:      id,
		Removed: true,
	})
	// TODO(tsandall): need to encode slashes in id?
	key := []byte(fmt.Sprintf("/policies/%v", id))
	return txn.underlying.Delete(key)
}

func (txn *transaction) readScan(path storage.Path) (interface{}, error) {

	prefix := []byte(path.String() + "/") // append / to exclude substring matches

	it := txn.underlying.NewIterator(badger.IteratorOptions{
		Prefix: prefix,
	})

	defer it.Close()

	result := map[string]interface{}{}

	for it.Rewind(); it.Valid(); it.Next() {
		item := it.Item()
		subpath, ok := storage.ParsePath(string(item.Key()))
		if !ok {
			return nil, errInvalidKey
		}

		subpath = subpath[len(path):]

		err := item.Value(func(bs []byte) error {

			var val interface{}

			if err := json.Unmarshal(bs, &val); err != nil {
				return err
			}

			node := result

			for i := 0; i < len(subpath)-1; i++ {
				k := subpath[i]
				next, ok := node[k]
				if !ok {
					next = map[string]interface{}{}
					node[k] = next
				}

				// NOTE(tsandall): this assertion cannot fail because the hierarchy
				// is constructed here--a panic indicates a bug in this code.
				node = next.(map[string]interface{})
			}

			node[subpath[len(subpath)-1]] = val
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (txn *transaction) writeAdd(path storage.Path, value interface{}) error {
	path = getPrefixedPath("data", path)
	ops, err := txn.partitionWriteAdd(path, value)
	if err != nil {
		return err
	}

	for _, op := range ops {
		if op.delete {
			return errors.New("not implemented: write: add: deletion")
		}

		bs, err := json.Marshal(op.val)
		if err != nil {
			return err
		}
		err = txn.underlying.Set(op.key, bs)
		if err != nil {
			return err
		}
	}

	return nil
}

type partitionOp struct {
	key    []byte
	delete bool
	val    interface{}
}

func (txn *transaction) partitionWriteAdd(path storage.Path, value interface{}) ([]partitionOp, error) {

	for _, p := range txn.db.partitions {
		if p.HasPrefix(path) {
			return txn.partitionWriteAddMultiple(path, value)
		}
	}

	for _, p := range txn.db.partitions {
		if path.HasPrefix(p) {
			return txn.partitionWriteAddOne(path, value, len(p)+1)
		}
	}

	return nil, errUnknownPartition(path)
}

func (txn *transaction) partitionWriteAddMultiple(path storage.Path, value interface{}) ([]partitionOp, error) {

	var result []partitionOp

	for _, p := range txn.db.partitions {
		if p.HasPrefix(path) {
			x, err := ptr(value, p[len(path):])
			if err != nil {
				return nil, err
			} else if x == nil {
				continue
			}
			obj, ok := x.(map[string]interface{})
			if !ok {
				return nil, errValueUnpartionable(p)
			}
			for k, v := range obj {
				result = append(result, partitionOp{
					key: []byte(p.String() + "/" + k),
					val: v,
				})
			}
		}
	}

	return result, nil

}

func (txn *transaction) partitionWriteAddOne(path storage.Path, value interface{}, index int) ([]partitionOp, error) {

	// exact match - return one operation
	if len(path) == index {
		return []partitionOp{
			{
				key: []byte(path.String()),
				val: value,
			},
		}, nil
	}

	// prefix match - return one operation but perform read-modify-write
	key := []byte(path[:index].String())
	item, err := txn.underlying.Get(key)
	if err != nil {
		return nil, err
	}

	var modified interface{}

	err = item.Value(func(bs []byte) error {

		if err := util.Unmarshal(bs, &modified); err != nil {
			return err
		}

		node := modified

		for i := index; i < len(path)-1; i++ {
			obj, ok := node.(map[string]interface{})
			if !ok {
				return errInvalidNonLeafType
			}
			node, ok = obj[path[i]]
			if !ok {
				return errInvalidNonLeafKey
			}
		}

		obj, ok := node.(map[string]interface{})
		if !ok {
			return errInvalidNonLeafType
		}

		obj[path[len(path)-1]] = value

		return nil
	})

	if err != nil {
		return nil, err
	}

	return []partitionOp{
		{
			key: key,
			val: modified,
		},
	}, nil
}

func (txn *transaction) partitionRead(path storage.Path) ([]byte, storage.Path, bool, error) {

	for _, p := range txn.db.partitions {

		if p.HasPrefix(path) {
			return nil, nil, true, nil
		}

		if path.HasPrefix(p) {
			return []byte(path[:len(p)+1].String()), path[len(p)+1:], false, nil
		}

	}

	return nil, nil, false, errUnknownPartition(path)
}

func ptr(x interface{}, path storage.Path) (interface{}, error) {

	result := x

	for _, k := range path {
		obj, ok := result.(map[string]interface{})
		if !ok {
			return nil, nil
		}
		result, ok = obj[k]
		if !ok {
			return nil, nil
		}
	}

	return result, nil
}

func errValueUnpartionable(p storage.Path) *storage.Error {
	return &storage.Error{Code: storage.InternalErr, Message: fmt.Sprintf("value cannot be partitioned: %v", p)}
}

func getPrefixedPath(prefix string, path storage.Path) storage.Path {
	cpy := make(storage.Path, len(path)+1)
	cpy[0] = prefix
	for i := 0; i < len(path); i++ {
		cpy[i+1] = path[i]
	}
	return cpy
}
