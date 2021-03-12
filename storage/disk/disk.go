// Copyright 2021 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package disk

import (
	"context"
	"fmt"

	"github.com/open-policy-agent/opa/storage/internal/pluggable"

	"github.com/dgraph-io/badger"
	"github.com/open-policy-agent/opa/storage"
)

// TODO(tsandall): slow-path for unpartitioned data
// TODO(tsandall): disable badger logging
// TODO(tsandall): support multi-level partitioning for use cases like k8s
// TODO(tsandall): check that writes don't escape from the defined partitions
// TODO(tsandall): how to deal w/ overwrite? need to delete existing keys...
// TODO(tsandall): assert that partitions are disjoint
// TODO(tsandall): wrap badger errors before returning from exported functions

var errNotFound = &storage.Error{Code: storage.NotFoundErr}
var errInvalidNonLeafType = &storage.Error{Code: storage.InvalidPatchErr, Message: "invalid non-leaf node type"}
var errInvalidNonLeafKey = &storage.Error{Code: storage.InvalidPatchErr, Message: "invalid non-leaf node key"}
var errInvalidPatch = &storage.Error{Code: storage.InvalidPatchErr}
var errInvalidKey = &storage.Error{Code: storage.InternalErr, Message: "invalid key"}

func errUnknownPartition(path storage.Path) error {
	return &storage.Error{Code: storage.InternalErr, Message: fmt.Sprintf("unknown partition: %v", path)}
}

func New(dir string, partitions []storage.Path) (storage.Store, error) {
	db, err := badger.Open(badger.DefaultOptions(dir))
	if err != nil {
		return nil, err
	}
	cpy := make([]storage.Path, len(partitions))
	for i := range cpy {
		cpy[i] = getPrefixedPath("data", partitions[i])
	}
	d := &disk{db: db, partitions: cpy}
	return pluggable.New(func(ctx context.Context, xid uint64, write bool, context *storage.Context) (pluggable.Transaction, error) {
		return newTransaction(xid, write, db.NewTransaction(write), context, d), nil
	}), nil
}

type disk struct {
	db         *badger.DB
	partitions []storage.Path
}
