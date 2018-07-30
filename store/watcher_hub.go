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

import (
	"container/list"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	etcdErr "github.com/coreos/etcd/error"
)

// A watcherHub contains all subscribed watchers
// watchers is a map with watched path as key and watcher as value
// EventHistory keeps the old events for watcherHub. It is used to help
// watcher to get a continuous event history. Or a watcher might miss the
// event happens between the end of the first watch command and the start
// of the second command.
// 一个watcherHub包含了所有订阅的watchers
// watchers是一个map，它以watch的path作为key并且将watcher作为value
// EventHistory存储了watcherHub的old events，它可以帮助watcher获取一个
// 连续的event历史，否则watcher可能会遗漏第一次执行watch命令和第二次执行watch
// 命令之间发生的event
type watcherHub struct {
	// count must be the first element to keep 64-bit alignment for atomic
	// access

	count int64 // current number of watchers.

	mutex        sync.Mutex
	watchers     map[string]*list.List
	EventHistory *EventHistory
}

// newWatchHub creates a watcherHub. The capacity determines how many events we will
// keep in the eventHistory.
// capacity决定了我们在eventHistory中保存多少历史event
// Typically, we only need to keep a small size of history[smaller than 20K].
// 一般来说，我们只需要保存少量的历史，少于20K
// Ideally, it should smaller than 20K/s[max throughput] * 2 * 50ms[RTT] = 2000
func newWatchHub(capacity int) *watcherHub {
	return &watcherHub{
		watchers:     make(map[string]*list.List),
		EventHistory: newEventHistory(capacity),
	}
}

// Watch function returns a Watcher.
// If recursive is true, the first change after index under key will be sent to the event channel of the watcher.
// If recursive is false, the first change after index at key will be sent to the event channel of the watcher.
// If index is zero, watch will start from the current index + 1.
func (wh *watcherHub) watch(key string, recursive, stream bool, index, storeIndex uint64) (Watcher, *etcdErr.Error) {
	reportWatchRequest()
	event, err := wh.EventHistory.scan(key, recursive, index)

	if err != nil {
		err.Index = storeIndex
		return nil, err
	}

	w := &watcher{
		eventChan:  make(chan *Event, 100), // use a buffered channel
		recursive:  recursive,
		stream:     stream,
		sinceIndex: index,
		startIndex: storeIndex,
		hub:        wh,
	}

	wh.mutex.Lock()
	defer wh.mutex.Unlock()
	// If the event exists in the known history, append the EtcdIndex and return immediately
	if event != nil {
		ne := event.Clone()
		ne.EtcdIndex = storeIndex
		w.eventChan <- ne
		return w, nil
	}

	l, ok := wh.watchers[key]

	var elem *list.Element

	if ok { // add the new watcher to the back of the list
		elem = l.PushBack(w)
	} else { // create a new list and add the new watcher
		l = list.New()
		elem = l.PushBack(w)
		wh.watchers[key] = l
	}

	w.remove = func() {
		if w.removed { // avoid removing it twice
			return
		}
		w.removed = true
		l.Remove(elem)
		atomic.AddInt64(&wh.count, -1)
		reportWatcherRemoved()
		if l.Len() == 0 {
			delete(wh.watchers, key)
		}
	}

	atomic.AddInt64(&wh.count, 1)
	reportWatcherAdded()

	return w, nil
}

func (wh *watcherHub) add(e *Event) {
	e = wh.EventHistory.addEvent(e)
}

// notify function accepts an event and notify to the watchers.
// notify函数接收一个event并且通知watchers
func (wh *watcherHub) notify(e *Event) {
	e = wh.EventHistory.addEvent(e) // add event into the eventHistory

	segments := strings.Split(e.Node.Key, "/")

	currPath := "/"

	// walk through all the segments of the path and notify the watchers
	// if the path is "/foo/bar", it will notify watchers with path "/",
	// "/foo" and "/foo/bar"
	// 遍历path中的所有segments，并且通知watchers
	// 如果path为"/foo/bar"，则会对路径"/", "/foo"和"/foo/bar"的watcher进行通知

	for _, segment := range segments {
		currPath = path.Join(currPath, segment)
		// notify the watchers who interests in the changes of current path
		// 对当前path的改变感兴趣的watchers进行通知
		wh.notifyWatchers(e, currPath, false)
	}
}

func (wh *watcherHub) notifyWatchers(e *Event, nodePath string, deleted bool) {
	wh.mutex.Lock()
	defer wh.mutex.Unlock()

	l, ok := wh.watchers[nodePath]
	// 获取该nodePath上所有的watchers
	if ok {
		curr := l.Front()

		for curr != nil {
			next := curr.Next() // save reference to the next one in the list

			w, _ := curr.Value.(*watcher)

			originalPath := (e.Node.Key == nodePath)
			if (originalPath || !isHidden(nodePath, e.Node.Key)) && w.notify(e, originalPath, deleted) {
				// 不能移除stream watcher
				if !w.stream { // do not remove the stream watcher
					// if we successfully notify a watcher
					// we need to remove the watcher from the list
					// and decrease the counter
					// 如果我们成功通知了一个watcher
					// 我们需要从list中移除watcher并且减小counter
					w.removed = true
					l.Remove(curr)
					atomic.AddInt64(&wh.count, -1)
					reportWatcherRemoved()
				}
			}

			curr = next // update current to the next element in the list
		}

		if l.Len() == 0 {
			// if we have notified all watcher in the list
			// we can delete the list
			// 如果我们已经notify了list中所有的watchers，我们可以将该list删除
			delete(wh.watchers, nodePath)
		}
	}
}

// clone function clones the watcherHub and return the cloned one.
// only clone the static content. do not clone the current watchers.
func (wh *watcherHub) clone() *watcherHub {
	clonedHistory := wh.EventHistory.clone()

	return &watcherHub{
		EventHistory: clonedHistory,
	}
}

// isHidden checks to see if key path is considered hidden to watch path i.e. the
// last element is hidden or it's within a hidden directory
func isHidden(watchPath, keyPath string) bool {
	// When deleting a directory, watchPath might be deeper than the actual keyPath
	// For example, when deleting /foo we also need to notify watchers on /foo/bar.
	if len(watchPath) > len(keyPath) {
		return false
	}
	// if watch path is just a "/", after path will start without "/"
	// add a "/" to deal with the special case when watchPath is "/"
	afterPath := path.Clean("/" + keyPath[len(watchPath):])
	return strings.Contains(afterPath, "/_")
}
