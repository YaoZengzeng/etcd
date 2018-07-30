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

package store

type Watcher interface {
	EventChan() chan *Event
	StartIndex() uint64 // The EtcdIndex at which the Watcher was created
	Remove()
}

type watcher struct {
	eventChan  chan *Event
	stream     bool
	recursive  bool
	sinceIndex uint64
	startIndex uint64
	hub        *watcherHub
	removed    bool
	remove     func()
}

func (w *watcher) EventChan() chan *Event {
	return w.eventChan
}

func (w *watcher) StartIndex() uint64 {
	return w.startIndex
}

// notify function notifies the watcher. If the watcher interests in the given path,
// the function will return true.
// notify函数通知watcher，如果watcher对给定的路径敢兴趣，该函数就会返回true
func (w *watcher) notify(e *Event, originalPath bool, deleted bool) bool {
	// watcher is interested the path in three cases and under one condition
	// the condition is that the event happens after the watcher's sinceIndex
	// watcher在三种case以及一种condition下会对path感兴趣，该conditon就是event在watcher的
	// sinceIndex之后发生

	// 1. the path at which the event happens is the path the watcher is watching at.
	// For example if the watcher is watching at "/foo" and the event happens at "/foo",
	// the watcher must be interested in that event.

	// 2. the watcher is a recursive watcher, it interests in the event happens after
	// its watching path. For example if watcher A watches at "/foo" and it is a recursive
	// one, it will interest in the event happens at "/foo/bar".

	// 3. when we delete a directory, we need to force notify all the watchers who watches
	// at the file we need to delete.
	// For example a watcher is watching at "/foo/bar". And we deletes "/foo". The watcher
	// should get notified even if "/foo" is not the path it is watching.
	if (w.recursive || originalPath || deleted) && e.Index() >= w.sinceIndex {
		// We cannot block here if the eventChan capacity is full, otherwise
		// etcd will hang. eventChan capacity is full when the rate of
		// notifications are higher than our send rate.
		// If this happens, we close the channel.
		// 我们不能block，如果eventChan capacity为full，否则etcd会hang
		// 当rate of notifications高于send rate，eventChan capacity就会full
		// 如果发生这样的情况，我们就关闭channel
		select {
		// notify就是将event发送到watcher的eventChan中
		case w.eventChan <- e:
		default:
			// We have missed a notification. Remove the watcher.
			// Removing the watcher also closes the eventChan.
			// 如果我们错过了一个notification，移除这个watcher
			// 移除这个watcher也关闭了eventChan
			// remove是新建watcher时可以配置的函数
			w.remove()
		}
		return true
	}
	return false
}

// Remove removes the watcher from watcherHub
// The actual remove function is guaranteed to only be executed once
// 真正的remove函数保证只会被执行一次
func (w *watcher) Remove() {
	w.hub.mutex.Lock()
	defer w.hub.mutex.Unlock()

	close(w.eventChan)
	if w.remove != nil {
		w.remove()
	}
}

// nopWatcher is a watcher that receives nothing, always blocking.
type nopWatcher struct{}

func NewNopWatcher() Watcher                 { return &nopWatcher{} }
func (w *nopWatcher) EventChan() chan *Event { return nil }
func (w *nopWatcher) StartIndex() uint64     { return 0 }
func (w *nopWatcher) Remove()                {}
