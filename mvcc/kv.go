// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mvcc

import (
	"go.etcd.io/etcd/lease"
	"go.etcd.io/etcd/mvcc/backend"
	"go.etcd.io/etcd/mvcc/mvccpb"
	"go.etcd.io/etcd/pkg/traceutil"
)

type RangeOptions struct {
	Limit int64
	Rev   int64
	Count bool
}

type RangeResult struct {
	KVs   []mvccpb.KeyValue
	Rev   int64
	Count int
}

type ReadView interface {
	// FirstRev returns the first KV revision at the time of opening the txn.
	// After a compaction, the first revision increases to the compaction
	// revision.
	FirstRev() int64

	// Rev returns the revision of the KV at the time of opening the txn.
	Rev() int64

	// Range gets the keys in the range at rangeRev.
	// The returned rev is the current revision of the KV when the operation is executed.
	// If rangeRev <=0, range gets the keys at currentRev.
	// If `end` is nil, the request returns the key.
	// If `end` is not nil and not empty, it gets the keys in range [key, range_end).
	// If `end` is not nil and empty, it gets the keys greater than or equal to key.
	// Limit limits the number of keys returned.
	// If the required rev is compacted, ErrCompacted will be returned.
	Range(key, end []byte, ro RangeOptions) (r *RangeResult, err error)
}

// TxnRead represents a read-only transaction with operations that will not
// block other read transactions.
// TxnRead代表一个只读的transaction，它的操作不会阻塞其他的读操作
type TxnRead interface {
	ReadView
	// End marks the transaction is complete and ready to commit.
	End()
}

type WriteView interface {
	// DeleteRange deletes the given range from the store.
	// A deleteRange increases the rev of the store if any key in the range exists.
	// The number of key deleted will be returned.
	// The returned rev is the current revision of the KV when the operation is executed.
	// It also generates one event for each key delete in the event history.
	// if the `end` is nil, deleteRange deletes the key.
	// if the `end` is not nil, deleteRange deletes the keys in range [key, range_end).
	DeleteRange(key, end []byte) (n, rev int64)

	// Put puts the given key, value into the store. Put also takes additional argument lease to
	// attach a lease to a key-value pair as meta-data. KV implementation does not validate the lease
	// id.
	// Put将给定的key, value传入store，Put同时用额外的参数lease将一个lease和key-value关联在一起作为元数据，KV的实现
	// 不会检测lease id
	// A put also increases the rev of the store, and generates one event in the event history.
	// 一个put操作也会增加store的rev，并且在event history中产生一个event
	// The returned rev is the current revision of the KV when the operation is executed.
	// 返回的rev是在操作结束之后KV当前的revision
	Put(key, value []byte, lease lease.LeaseID) (rev int64)
}

// TxnWrite represents a transaction that can modify the store.
// TxnWrite代表会修改store的transaction
type TxnWrite interface {
	TxnRead
	WriteView
	// Changes gets the changes made since opening the write txn.
	Changes() []mvccpb.KeyValue
}

// txnReadWrite coerces a read txn to a write, panicking on any write operation.
type txnReadWrite struct{ TxnRead }

func (trw *txnReadWrite) DeleteRange(key, end []byte) (n, rev int64) { panic("unexpected DeleteRange") }
func (trw *txnReadWrite) Put(key, value []byte, lease lease.LeaseID) (rev int64) {
	panic("unexpected Put")
}
func (trw *txnReadWrite) Changes() []mvccpb.KeyValue { return nil }

func NewReadOnlyTxnWrite(txn TxnRead) TxnWrite { return &txnReadWrite{txn} }

type KV interface {
	ReadView
	WriteView

	// Read creates a read transaction.
	// Read创建一个read transaction
	Read(trace *traceutil.Trace) TxnRead

	// Write creates a write transaction.
	Write(trace *traceutil.Trace) TxnWrite

	// Hash computes the hash of the KV's backend.
	Hash() (hash uint32, revision int64, err error)

	// HashByRev computes the hash of all MVCC revisions up to a given revision.
	HashByRev(rev int64) (hash uint32, revision int64, compactRev int64, err error)

	// Compact frees all superseded keys with revisions less than rev.
	// Compact将所有revisions小于rev的keys都清除掉
	Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error)

	// Commit commits outstanding txns into the underlying backend.
	// Commit将外部的txns提交到底层的backend
	Commit()

	// Restore restores the KV store from a backend.
	// Restore从一个backend中重新恢复KV
	Restore(b backend.Backend) error
	Close() error
}

// WatchableKV is a KV that can be watched.
// WatchableKV是一个能够被监听的KV
type WatchableKV interface {
	KV
	Watchable
}

// Watchable is the interface that wraps the NewWatchStream function.
// Watchable是封装了NewWatchStream功能的接口
type Watchable interface {
	// NewWatchStream returns a WatchStream that can be used to
	// watch events happened or happening on the KV.
	// NewWatchStream返回一个WatchStream，它可以用来监听KV中已经发生或者正在发生的events
	NewWatchStream() WatchStream
}

// ConsistentWatchableKV is a WatchableKV that understands the consistency
// algorithm and consistent index.
// ConsistentWatchableKV是一个WatchableKV，它理解共识算法以及consistent index
// If the consistent index of executing entry is not larger than the
// consistent index of ConsistentWatchableKV, all operations in
// this entry are skipped and return empty response.
// 如果executing entry的consistent index不大于ConsistentWatchableKV的consistent index
// 则这个entry的所有操作都会跳过并且返回空的response
type ConsistentWatchableKV interface {
	WatchableKV
	// ConsistentIndex returns the current consistent index of the KV.
	ConsistentIndex() uint64
}
