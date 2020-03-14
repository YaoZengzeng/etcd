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
	"bytes"
	"errors"
	"sync"

	"go.etcd.io/etcd/mvcc/mvccpb"
)

// AutoWatchID is the watcher ID passed in WatchStream.Watch when no
// user-provided ID is available. If pass, an ID will automatically be assigned.
const AutoWatchID WatchID = 0

var (
	ErrWatcherNotExist    = errors.New("mvcc: watcher does not exist")
	ErrEmptyWatcherRange  = errors.New("mvcc: watcher range is empty")
	ErrWatcherDuplicateID = errors.New("mvcc: duplicate watch ID provided on the WatchStream")
)

type WatchID int64

// FilterFunc returns true if the given event should be filtered out.
// FilterFunc返回true，如果给定的event应该被过滤
type FilterFunc func(e mvccpb.Event) bool

type WatchStream interface {
	// Watch creates a watcher. The watcher watches the events happening or
	// happened on the given key or range [key, end) from the given startRev.
	// Watch创建一个watcher，watcher监听正在发生或者已经发生的events，对于给定的key或者range [key, end)
	// 从startRev开始
	//
	// The whole event history can be watched unless compacted.
	// If "startRev" <=0, watch observes events after currentRev.
	// 整个的event history都能被监听，除非被压缩了，如果"startRev"小于等于0，watch从currentRev
	// 开始发现events
	//
	// The returned "id" is the ID of this watcher. It appears as WatchID
	// in events that are sent to the created watcher through stream channel.
	// The watch ID is used when it's not equal to AutoWatchID. Otherwise,
	// an auto-generated watch ID is returned.
	Watch(id WatchID, key, end []byte, startRev int64, fcs ...FilterFunc) (WatchID, error)

	// Chan returns a chan. All watch response will be sent to the returned chan.
	// Chan返回一个chan，所有的watch response都会被发送到返回到chan
	Chan() <-chan WatchResponse

	// RequestProgress requests the progress of the watcher with given ID. The response
	// will only be sent if the watcher is currently synced.
	// The responses will be sent through the WatchRespone Chan attached
	// with this stream to ensure correct ordering.
	// The responses contains no events. The revision in the response is the progress
	// of the watchers since the watcher is currently synced.
	RequestProgress(id WatchID)

	// Cancel cancels a watcher by giving its ID. If watcher does not exist, an error will be
	// returned.
	Cancel(id WatchID) error

	// Close closes Chan and release all related resources.
	Close()

	// Rev returns the current revision of the KV the stream watches on.
	// Rev返回stream监听的KV的current revision
	Rev() int64
}

type WatchResponse struct {
	// WatchID is the WatchID of the watcher this response sent to.
	// WatchID是这个response发往的WatchID
	WatchID WatchID

	// Events contains all the events that needs to send.
	// Events包含了所有需要发送的events
	Events []mvccpb.Event

	// Revision is the revision of the KV when the watchResponse is created.
	// Revision是当watchResponse被创建时KV的revision
	// For a normal response, the revision should be the same as the last
	// modified revision inside Events. For a delayed response to a unsynced
	// watcher, the revision is greater than the last modified revision
	// inside Events.
	// 对于normal response，revision应该和Events中最后一次修改的revision相同
	// 对于一个unsynced watcher，revision应该比Events中最后修改的revision要大
	Revision int64

	// CompactRevision is set when the watcher is cancelled due to compaction.
	// 当watcher因为compaction被取消时，CompactRevision会被设置
	CompactRevision int64
}

// watchStream contains a collection of watchers that share
// one streaming chan to send out watched events and other control events.
// watchStream包含了一系列的watchers，它们共享一个streaming channel用来发送watched events以及
// 其他的control events
type watchStream struct {
	watchable watchable
	ch        chan WatchResponse

	mu sync.Mutex // guards fields below it
	// nextID is the ID pre-allocated for next new watcher in this stream
	// nextID是这个stream中为下一个新的watcher提前申请的watcher
	nextID   WatchID
	closed   bool
	// 根据WatchID分别分配一个cancelFunc和watcher
	cancels  map[WatchID]cancelFunc
	watchers map[WatchID]*watcher
}

// Watch creates a new watcher in the stream and returns its WatchID.
// Watch在stream创建一个新的watcher并且返回它的WatchID
func (ws *watchStream) Watch(id WatchID, key, end []byte, startRev int64, fcs ...FilterFunc) (WatchID, error) {
	// prevent wrong range where key >= end lexicographically
	// watch request with 'WithFromKey' has empty-byte range end
	if len(end) != 0 && bytes.Compare(key, end) != -1 {
		return -1, ErrEmptyWatcherRange
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closed {
		return -1, ErrEmptyWatcherRange
	}

	if id == AutoWatchID {
		for ws.watchers[ws.nextID] != nil {
			ws.nextID++
		}
		id = ws.nextID
		ws.nextID++
	} else if _, ok := ws.watchers[id]; ok {
		return -1, ErrWatcherDuplicateID
	}

	// 调用watchable创建watcher
	w, c := ws.watchable.watch(key, end, startRev, id, ws.ch, fcs...)

	ws.cancels[id] = c
	ws.watchers[id] = w
	return id, nil
}

func (ws *watchStream) Chan() <-chan WatchResponse {
	return ws.ch
}

func (ws *watchStream) Cancel(id WatchID) error {
	ws.mu.Lock()
	cancel, ok := ws.cancels[id]
	w := ws.watchers[id]
	ok = ok && !ws.closed
	ws.mu.Unlock()

	if !ok {
		return ErrWatcherNotExist
	}
	cancel()

	ws.mu.Lock()
	// The watch isn't removed until cancel so that if Close() is called,
	// it will wait for the cancel. Otherwise, Close() could close the
	// watch channel while the store is still posting events.
	if ww := ws.watchers[id]; ww == w {
		delete(ws.cancels, id)
		delete(ws.watchers, id)
	}
	ws.mu.Unlock()

	return nil
}

func (ws *watchStream) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for _, cancel := range ws.cancels {
		cancel()
	}
	ws.closed = true
	close(ws.ch)
	watchStreamGauge.Dec()
}

func (ws *watchStream) Rev() int64 {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.watchable.rev()
}

func (ws *watchStream) RequestProgress(id WatchID) {
	ws.mu.Lock()
	w, ok := ws.watchers[id]
	ws.mu.Unlock()
	if !ok {
		return
	}
	ws.watchable.progress(w)
}
