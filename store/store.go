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
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	etcdErr "github.com/coreos/etcd/error"
	"github.com/coreos/etcd/pkg/types"
	"github.com/jonboulle/clockwork"
)

// The default version to set when the store is first initialized.
const defaultVersion = 2

var minExpireTime time.Time

func init() {
	minExpireTime, _ = time.Parse(time.RFC3339, "2000-01-01T00:00:00Z")
}

type Store interface {
	Version() int
	Index() uint64

	Get(nodePath string, recursive, sorted bool) (*Event, error)
	Set(nodePath string, dir bool, value string, expireOpts TTLOptionSet) (*Event, error)
	Update(nodePath string, newValue string, expireOpts TTLOptionSet) (*Event, error)
	Create(nodePath string, dir bool, value string, unique bool,
		expireOpts TTLOptionSet) (*Event, error)
	CompareAndSwap(nodePath string, prevValue string, prevIndex uint64,
		value string, expireOpts TTLOptionSet) (*Event, error)
	Delete(nodePath string, dir, recursive bool) (*Event, error)
	CompareAndDelete(nodePath string, prevValue string, prevIndex uint64) (*Event, error)

	Watch(prefix string, recursive, stream bool, sinceIndex uint64) (Watcher, error)

	Save() ([]byte, error)
	Recovery(state []byte) error

	Clone() Store
	SaveNoCopy() ([]byte, error)

	JsonStats() []byte
	DeleteExpiredKeys(cutoff time.Time)

	HasTTLKeys() bool
}

type TTLOptionSet struct {
	ExpireTime time.Time
	Refresh    bool
}

type store struct {
	Root           *node
	WatcherHub     *watcherHub
	// 每次变更，index会加1，每个event都会关联到current index
	CurrentIndex   uint64
	// Stats为一系列统计数据
	Stats          *Stats
	CurrentVersion int
	ttlKeyHeap     *ttlKeyHeap  // need to recovery manually
	worldLock      sync.RWMutex // stop the world lock
	clock          clockwork.Clock
	readonlySet    types.Set
}

// New creates a store where the given namespaces will be created as initial directories.
// New会新建一个store，给定的目录会被创建为初始化目录
func New(namespaces ...string) Store {
	s := newStore(namespaces...)
	s.clock = clockwork.NewRealClock()
	return s
}

func newStore(namespaces ...string) *store {
	s := new(store)
	s.CurrentVersion = defaultVersion
	// 创建根目录，根目录的parent为nil
	s.Root = newDir(s, "/", s.CurrentIndex, nil, Permanent)
	for _, namespace := range namespaces {
		// namespace是就是node path，加入根节点中
		s.Root.Add(newDir(s, namespace, s.CurrentIndex, s.Root, Permanent))
	}
	s.Stats = newStats()
	s.WatcherHub = newWatchHub(1000)
	s.ttlKeyHeap = newTtlKeyHeap()
	// 将namespaces以及根目录添加到readonly set中
	s.readonlySet = types.NewUnsafeSet(append(namespaces, "/")...)
	return s
}

// Version retrieves current version of the store.
func (s *store) Version() int {
	return s.CurrentVersion
}

// Index retrieves the current index of the store.
func (s *store) Index() uint64 {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()
	return s.CurrentIndex
}

// Get returns a get event.
// Get返回一个get event
// If recursive is true, it will return all the content under the node path.
// 如果recursive为true，它会返回该node path下所有的内容
// If sorted is true, it will sort the content by keys.
// 如果sorted为true，它会按key对结果进排序
func (s *store) Get(nodePath string, recursive, sorted bool) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.RLock()
	defer s.worldLock.RUnlock()

	defer func() {
		// defer函数对store中的Stats进行更新
		if err == nil {
			s.Stats.Inc(GetSuccess)
			if recursive {
				reportReadSuccess(GetRecursive)
			} else {
				reportReadSuccess(Get)
			}
			return
		}

		s.Stats.Inc(GetFail)
		if recursive {
			reportReadFailure(GetRecursive)
		} else {
			reportReadFailure(Get)
		}
	}()

	n, err := s.internalGet(nodePath)
	if err != nil {
		return nil, err
	}

	e := newEvent(Get, nodePath, n.ModifiedIndex, n.CreatedIndex)
	e.EtcdIndex = s.CurrentIndex
	// 加载填充Node的内容
	e.Node.loadInternalNode(n, recursive, sorted, s.clock)

	return e, nil
}

// Create creates the node at nodePath. Create will help to create intermediate directories with no ttl.
// Create在nodePath创建node，Create可以用来创建没有ttl的中间目录
// If the node has already existed, create will fail.
// 如果node已经存在了，则create会失败
// If any node on the path is a file, create will fail.
// 如果path中的任何node是一个文件，则create也会失败
func (s *store) Create(nodePath string, dir bool, value string, unique bool, expireOpts TTLOptionSet) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(CreateSuccess)
			reportWriteSuccess(Create)
			return
		}

		s.Stats.Inc(CreateFail)
		reportWriteFailure(Create)
	}()

	// 内部创建节点，生成一个event
	e, err := s.internalCreate(nodePath, dir, value, unique, false, expireOpts.ExpireTime, Create)
	if err != nil {
		return nil, err
	}

	e.EtcdIndex = s.CurrentIndex
	s.WatcherHub.notify(e)

	return e, nil
}

// Set creates or replace the node at nodePath.
func (s *store) Set(nodePath string, dir bool, value string, expireOpts TTLOptionSet) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(SetSuccess)
			reportWriteSuccess(Set)
			return
		}

		s.Stats.Inc(SetFail)
		reportWriteFailure(Set)
	}()

	// Get prevNode value
	n, getErr := s.internalGet(nodePath)
	if getErr != nil && getErr.ErrorCode != etcdErr.EcodeKeyNotFound {
		err = getErr
		return nil, err
	}

	if expireOpts.Refresh {
		if getErr != nil {
			err = getErr
			return nil, err
		} else {
			value = n.Value
		}
	}

	// Set new value
	e, err := s.internalCreate(nodePath, dir, value, false, true, expireOpts.ExpireTime, Set)
	if err != nil {
		return nil, err
	}
	e.EtcdIndex = s.CurrentIndex

	// Put prevNode into event
	if getErr == nil {
		prev := newEvent(Get, nodePath, n.ModifiedIndex, n.CreatedIndex)
		prev.Node.loadInternalNode(n, false, false, s.clock)
		e.PrevNode = prev.Node
	}

	if !expireOpts.Refresh {
		s.WatcherHub.notify(e)
	} else {
		e.SetRefresh()
		s.WatcherHub.add(e)
	}

	return e, nil
}

// returns user-readable cause of failed comparison
func getCompareFailCause(n *node, which int, prevValue string, prevIndex uint64) string {
	switch which {
	case CompareIndexNotMatch:
		return fmt.Sprintf("[%v != %v]", prevIndex, n.ModifiedIndex)
	case CompareValueNotMatch:
		return fmt.Sprintf("[%v != %v]", prevValue, n.Value)
	default:
		return fmt.Sprintf("[%v != %v] [%v != %v]", prevValue, n.Value, prevIndex, n.ModifiedIndex)
	}
}

func (s *store) CompareAndSwap(nodePath string, prevValue string, prevIndex uint64,
	value string, expireOpts TTLOptionSet) (*Event, error) {

	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(CompareAndSwapSuccess)
			reportWriteSuccess(CompareAndSwap)
			return
		}

		s.Stats.Inc(CompareAndSwapFail)
		reportWriteFailure(CompareAndSwap)
	}()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if s.readonlySet.Contains(nodePath) {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.CurrentIndex)
	}

	n, err := s.internalGet(nodePath)
	if err != nil {
		return nil, err
	}
	if n.IsDir() { // can only compare and swap file
		err = etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, s.CurrentIndex)
		return nil, err
	}

	// If both of the prevValue and prevIndex are given, we will test both of them.
	// Command will be executed, only if both of the tests are successful.
	if ok, which := n.Compare(prevValue, prevIndex); !ok {
		cause := getCompareFailCause(n, which, prevValue, prevIndex)
		err = etcdErr.NewError(etcdErr.EcodeTestFailed, cause, s.CurrentIndex)
		return nil, err
	}

	if expireOpts.Refresh {
		value = n.Value
	}

	// update etcd index
	s.CurrentIndex++

	e := newEvent(CompareAndSwap, nodePath, s.CurrentIndex, n.CreatedIndex)
	e.EtcdIndex = s.CurrentIndex
	e.PrevNode = n.Repr(false, false, s.clock)
	eNode := e.Node

	// if test succeed, write the value
	n.Write(value, s.CurrentIndex)
	n.UpdateTTL(expireOpts.ExpireTime)

	// copy the value for safety
	valueCopy := value
	eNode.Value = &valueCopy
	eNode.Expiration, eNode.TTL = n.expirationAndTTL(s.clock)

	if !expireOpts.Refresh {
		s.WatcherHub.notify(e)
	} else {
		e.SetRefresh()
		s.WatcherHub.add(e)
	}

	return e, nil
}

// Delete deletes the node at the given path.
// If the node is a directory, recursive must be true to delete it.
func (s *store) Delete(nodePath string, dir, recursive bool) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(DeleteSuccess)
			reportWriteSuccess(Delete)
			return
		}

		s.Stats.Inc(DeleteFail)
		reportWriteFailure(Delete)
	}()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if s.readonlySet.Contains(nodePath) {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.CurrentIndex)
	}

	// recursive implies dir
	if recursive {
		dir = true
	}

	n, err := s.internalGet(nodePath)
	if err != nil { // if the node does not exist, return error
		return nil, err
	}

	nextIndex := s.CurrentIndex + 1
	e := newEvent(Delete, nodePath, nextIndex, n.CreatedIndex)
	e.EtcdIndex = nextIndex
	e.PrevNode = n.Repr(false, false, s.clock)
	eNode := e.Node

	if n.IsDir() {
		eNode.Dir = true
	}

	callback := func(path string) { // notify function
		// notify the watchers with deleted set true
		s.WatcherHub.notifyWatchers(e, path, true)
	}

	err = n.Remove(dir, recursive, callback)
	if err != nil {
		return nil, err
	}

	// update etcd index
	s.CurrentIndex++

	s.WatcherHub.notify(e)

	return e, nil
}

func (s *store) CompareAndDelete(nodePath string, prevValue string, prevIndex uint64) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(CompareAndDeleteSuccess)
			reportWriteSuccess(CompareAndDelete)
			return
		}

		s.Stats.Inc(CompareAndDeleteFail)
		reportWriteFailure(CompareAndDelete)
	}()

	nodePath = path.Clean(path.Join("/", nodePath))

	n, err := s.internalGet(nodePath)
	if err != nil { // if the node does not exist, return error
		return nil, err
	}
	if n.IsDir() { // can only compare and delete file
		return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, s.CurrentIndex)
	}

	// If both of the prevValue and prevIndex are given, we will test both of them.
	// Command will be executed, only if both of the tests are successful.
	if ok, which := n.Compare(prevValue, prevIndex); !ok {
		cause := getCompareFailCause(n, which, prevValue, prevIndex)
		return nil, etcdErr.NewError(etcdErr.EcodeTestFailed, cause, s.CurrentIndex)
	}

	// update etcd index
	s.CurrentIndex++

	e := newEvent(CompareAndDelete, nodePath, s.CurrentIndex, n.CreatedIndex)
	e.EtcdIndex = s.CurrentIndex
	e.PrevNode = n.Repr(false, false, s.clock)

	callback := func(path string) { // notify function
		// notify the watchers with deleted set true
		s.WatcherHub.notifyWatchers(e, path, true)
	}

	err = n.Remove(false, false, callback)
	if err != nil {
		return nil, err
	}

	s.WatcherHub.notify(e)

	return e, nil
}

func (s *store) Watch(key string, recursive, stream bool, sinceIndex uint64) (Watcher, error) {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()

	key = path.Clean(path.Join("/", key))
	if sinceIndex == 0 {
		sinceIndex = s.CurrentIndex + 1
	}
	// WatcherHub does not know about the current index, so we need to pass it in
	w, err := s.WatcherHub.watch(key, recursive, stream, sinceIndex, s.CurrentIndex)
	if err != nil {
		return nil, err
	}

	return w, nil
}

// walk walks all the nodePath and apply the walkFunc on each directory
// walk遍历nodePath，对每个目录执行walkFunc函数
func (s *store) walk(nodePath string, walkFunc func(prev *node, component string) (*node, *etcdErr.Error)) (*node, *etcdErr.Error) {
	components := strings.Split(nodePath, "/")

	curr := s.Root
	var err *etcdErr.Error

	for i := 1; i < len(components); i++ {
		if len(components[i]) == 0 { // ignore empty string
			return curr, nil
		}

		// 对路径中的每个元素执行walkFunc函数
		curr, err = walkFunc(curr, components[i])
		if err != nil {
			return nil, err
		}
	}

	return curr, nil
}

// Update updates the value/ttl of the node.
// If the node is a file, the value and the ttl can be updated.
// If the node is a directory, only the ttl can be updated.
func (s *store) Update(nodePath string, newValue string, expireOpts TTLOptionSet) (*Event, error) {
	var err *etcdErr.Error

	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	defer func() {
		if err == nil {
			s.Stats.Inc(UpdateSuccess)
			reportWriteSuccess(Update)
			return
		}

		s.Stats.Inc(UpdateFail)
		reportWriteFailure(Update)
	}()

	nodePath = path.Clean(path.Join("/", nodePath))
	// we do not allow the user to change "/"
	if s.readonlySet.Contains(nodePath) {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", s.CurrentIndex)
	}

	currIndex, nextIndex := s.CurrentIndex, s.CurrentIndex+1

	n, err := s.internalGet(nodePath)
	if err != nil { // if the node does not exist, return error
		return nil, err
	}
	if n.IsDir() && len(newValue) != 0 {
		// if the node is a directory, we cannot update value to non-empty
		return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, currIndex)
	}

	if expireOpts.Refresh {
		newValue = n.Value
	}

	e := newEvent(Update, nodePath, nextIndex, n.CreatedIndex)
	e.EtcdIndex = nextIndex
	e.PrevNode = n.Repr(false, false, s.clock)
	eNode := e.Node

	n.Write(newValue, nextIndex)

	if n.IsDir() {
		eNode.Dir = true
	} else {
		// copy the value for safety
		newValueCopy := newValue
		eNode.Value = &newValueCopy
	}

	// update ttl
	n.UpdateTTL(expireOpts.ExpireTime)

	eNode.Expiration, eNode.TTL = n.expirationAndTTL(s.clock)

	if !expireOpts.Refresh {
		s.WatcherHub.notify(e)
	} else {
		e.SetRefresh()
		s.WatcherHub.add(e)
	}

	s.CurrentIndex = nextIndex

	return e, nil
}

func (s *store) internalCreate(nodePath string, dir bool, value string, unique, replace bool,
	expireTime time.Time, action string) (*Event, *etcdErr.Error) {

	currIndex, nextIndex := s.CurrentIndex, s.CurrentIndex+1

	if unique { // append unique item under the node path
		nodePath += "/" + fmt.Sprintf("%020s", strconv.FormatUint(nextIndex, 10))
	}

	nodePath = path.Clean(path.Join("/", nodePath))

	// we do not allow the user to change "/"
	// 我们不允许用户改变"/"
	if s.readonlySet.Contains(nodePath) {
		return nil, etcdErr.NewError(etcdErr.EcodeRootROnly, "/", currIndex)
	}

	// Assume expire times that are way in the past are
	// This can occur when the time is serialized to JS
	// 如果expireTime早于minExpireTime，则将其设置为Permanent
	if expireTime.Before(minExpireTime) {
		expireTime = Permanent
	}

	dirName, nodeName := path.Split(nodePath)

	// walk through the nodePath, create dirs and get the last directory node
	// 遍历nodePath，创建目录并且获取最后一个directory node
	d, err := s.walk(dirName, s.checkDir)

	if err != nil {
		s.Stats.Inc(SetFail)
		reportWriteFailure(action)
		err.Index = currIndex
		return nil, err
	}

	e := newEvent(action, nodePath, nextIndex, nextIndex)
	eNode := e.Node

	n, _ := d.GetChild(nodeName)

	// force will try to replace an existing file
	if n != nil {
		if replace {
			if n.IsDir() {
				return nil, etcdErr.NewError(etcdErr.EcodeNotFile, nodePath, currIndex)
			}
			e.PrevNode = n.Repr(false, false, s.clock)

			n.Remove(false, false, nil)
		} else {
			// 如果Node已经存在，且不替换
			return nil, etcdErr.NewError(etcdErr.EcodeNodeExist, nodePath, currIndex)
		}
	}

	if !dir { // create file
		// copy the value for safety
		valueCopy := value
		eNode.Value = &valueCopy

		// 调用newKV创建file
		n = newKV(s, nodePath, value, nextIndex, d, expireTime)

	} else { // create directory
		eNode.Dir = true
		// 调用newDir创建目录
		n = newDir(s, nodePath, nextIndex, d, expireTime)
	}

	// we are sure d is a directory and does not have the children with name n.Name
	// 我们确保d是一个目录并且没有children的name是n.Name
	d.Add(n)

	// node with TTL
	if !n.IsPermanent() {
		// 如果节点不是permanent，加入ttlKeyHeap
		s.ttlKeyHeap.push(n)

		eNode.Expiration, eNode.TTL = n.expirationAndTTL(s.clock)
	}

	// 更新store的CurrentIndex
	s.CurrentIndex = nextIndex

	return e, nil
}

// InternalGet gets the node of the given nodePath.
// InternalGet获取指定nodePath的node
func (s *store) internalGet(nodePath string) (*node, *etcdErr.Error) {
	nodePath = path.Clean(path.Join("/", nodePath))

	walkFunc := func(parent *node, name string) (*node, *etcdErr.Error) {

		if !parent.IsDir() {
			err := etcdErr.NewError(etcdErr.EcodeNotDir, parent.Path, s.CurrentIndex)
			return nil, err
		}

		child, ok := parent.Children[name]
		if ok {
			return child, nil
		}

		return nil, etcdErr.NewError(etcdErr.EcodeKeyNotFound, path.Join(parent.Path, name), s.CurrentIndex)
	}

	f, err := s.walk(nodePath, walkFunc)

	if err != nil {
		return nil, err
	}
	return f, nil
}

// DeleteExpiredKeys will delete all expired keys
// DeleteExpiredKeys会删除所有过期的keys
func (s *store) DeleteExpiredKeys(cutoff time.Time) {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()

	for {
		node := s.ttlKeyHeap.top()
		if node == nil || node.ExpireTime.After(cutoff) {
			break
		}

		s.CurrentIndex++
		// 创建过期的event
		e := newEvent(Expire, node.Path, s.CurrentIndex, node.CreatedIndex)
		e.EtcdIndex = s.CurrentIndex
		e.PrevNode = node.Repr(false, false, s.clock)

		callback := func(path string) { // notify function
			// notify the watchers with deleted set true
			// 通知watchers并且将deleted设置为true
			s.WatcherHub.notifyWatchers(e, path, true)
		}

		s.ttlKeyHeap.pop()
		// dir, recursive都设置为true
		node.Remove(true, true, callback)

		reportExpiredKey()
		s.Stats.Inc(ExpireCount)

		s.WatcherHub.notify(e)
	}

}

// checkDir will check whether the component is a directory under parent node.
// checkDir会检查parent node下的组件是否为目录
// If it is a directory, this function will return the pointer to that node.
// 如果是一个目录，则返回该节点的pointer
// If it does not exist, this function will create a new directory and return the pointer to that node.
// 如果它不存在，该函数会创建一个新的目录并且返回指向该节点的指针
// If it is a file, this function will return error.
// 如果它是一个文件，则返回一个error
func (s *store) checkDir(parent *node, dirName string) (*node, *etcdErr.Error) {
	node, ok := parent.Children[dirName]

	if ok {
		if node.IsDir() {
			return node, nil
		}

		return nil, etcdErr.NewError(etcdErr.EcodeNotDir, node.Path, s.CurrentIndex)
	}

	// 如果目录节点不存在，就创建一个新的，并返回
	n := newDir(s, path.Join(parent.Path, dirName), s.CurrentIndex+1, parent, Permanent)

	parent.Children[dirName] = n

	return n, nil
}

// Save saves the static state of the store system.
// It will not be able to save the state of watchers.
// It will not save the parent field of the node. Or there will
// be cyclic dependencies issue for the json package.
func (s *store) Save() ([]byte, error) {
	b, err := json.Marshal(s.Clone())
	if err != nil {
		return nil, err
	}

	return b, nil
}

func (s *store) SaveNoCopy() ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func (s *store) Clone() Store {
	s.worldLock.Lock()

	clonedStore := newStore()
	clonedStore.CurrentIndex = s.CurrentIndex
	clonedStore.Root = s.Root.Clone()
	clonedStore.WatcherHub = s.WatcherHub.clone()
	clonedStore.Stats = s.Stats.clone()
	clonedStore.CurrentVersion = s.CurrentVersion

	s.worldLock.Unlock()
	return clonedStore
}

// Recovery recovers the store system from a static state
// It needs to recover the parent field of the nodes.
// It needs to delete the expired nodes since the saved time and also
// needs to create monitoring go routines.
func (s *store) Recovery(state []byte) error {
	s.worldLock.Lock()
	defer s.worldLock.Unlock()
	err := json.Unmarshal(state, s)

	if err != nil {
		return err
	}

	s.ttlKeyHeap = newTtlKeyHeap()

	s.Root.recoverAndclean()
	return nil
}

func (s *store) JsonStats() []byte {
	s.Stats.Watchers = uint64(s.WatcherHub.count)
	return s.Stats.toJson()
}

func (s *store) HasTTLKeys() bool {
	s.worldLock.RLock()
	defer s.worldLock.RUnlock()
	return s.ttlKeyHeap.Len() != 0
}
