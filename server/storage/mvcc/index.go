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
	"sort"
	"sync"

	"github.com/google/btree"
	"go.uber.org/zap"
)

type index interface {
	Get(key []byte, atRev int64) (rev, created revision, ver int64, err error)
	Range(key, end []byte, atRev int64) ([][]byte, []revision)
	Revisions(key, end []byte, atRev int64, limit int) ([]revision, int)
	CountRevisions(key, end []byte, atRev int64) int
	Put(key []byte, rev revision)
	Tombstone(key []byte, rev revision) error
	RangeSince(key, end []byte, rev int64) []revision
	Compact(rev int64) map[revision]struct{}
	Keep(rev int64) map[revision]struct{}
	Equal(b index) bool

	Insert(ki *keyIndex)
	KeyIndex(ki *keyIndex) *keyIndex
}

type treeIndex struct {
	sync.RWMutex
	tree *btree.BTree
	lg   *zap.Logger
}

func newTreeIndex(lg *zap.Logger) index {
	return &treeIndex{
		tree: btree.New(32),
		lg:   lg,
	}
}

func (ti *treeIndex) Put(key []byte, rev revision) {
	keyi := &keyIndex{key: key}

	ti.Lock()
	defer ti.Unlock()
	item := ti.tree.Get(keyi)
	if item == nil {
		keyi.put(ti.lg, rev.main, rev.sub)
		ti.tree.ReplaceOrInsert(keyi)
		return
	}
	okeyi := item.(*keyIndex)
	okeyi.put(ti.lg, rev.main, rev.sub)
}

func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created revision, ver int64, err error) {
	ti.RLock()
	defer ti.RUnlock()
	return ti.unsafeGet(key, atRev)
}

func (ti *treeIndex) unsafeGet(key []byte, atRev int64) (modified, created revision, ver int64, err error) {
	keyi := &keyIndex{key: key}
	if keyi = ti.keyIndex(keyi); keyi == nil {
		return revision{}, revision{}, 0, ErrRevisionNotFound
	}
	return keyi.get(ti.lg, atRev)
}

func (ti *treeIndex) KeyIndex(keyi *keyIndex) *keyIndex {
	ti.RLock()
	defer ti.RUnlock()
	return ti.keyIndex(keyi)
}

func (ti *treeIndex) keyIndex(keyi *keyIndex) *keyIndex {
	if item := ti.tree.Get(keyi); item != nil {
		return item.(*keyIndex)
	}
	return nil
}

func (ti *treeIndex) unsafeVisit(key, end []byte, f func(ki *keyIndex) bool) {
	keyi, endi := &keyIndex{key: key}, &keyIndex{key: end}

	ti.tree.AscendGreaterOrEqual(keyi, func(item btree.Item) bool {
		if len(endi.key) > 0 && !item.Less(endi) {
			return false
		}
		if !f(item.(*keyIndex)) {
			return false
		}
		return true
	})
}

func (ti *treeIndex) Revisions(key, end []byte, atRev int64, limit int) (revs []revision, total int) {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		rev, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return nil, 0
		}
		return []revision{rev}, 1
	}
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
			if limit <= 0 || len(revs) < limit {
				revs = append(revs, rev)
			}
			total++
		}
		return true
	})
	return revs, total
}

func (ti *treeIndex) CountRevisions(key, end []byte, atRev int64) int {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		_, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return 0
		}
		return 1
	}
	total := 0
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if _, _, _, err := ki.get(ti.lg, atRev); err == nil {
			total++
		}
		return true
	})
	return total
}

func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []revision) {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		rev, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return nil, nil
		}
		return [][]byte{key}, []revision{rev}
	}
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
			revs = append(revs, rev)
			keys = append(keys, ki.key)
		}
		return true
	})
	return keys, revs
}

func (ti *treeIndex) Tombstone(key []byte, rev revision) error {
	keyi := &keyIndex{key: key}

	ti.Lock()
	defer ti.Unlock()
	item := ti.tree.Get(keyi)
	if item == nil {
		return ErrRevisionNotFound
	}

	ki := item.(*keyIndex)
	return ki.tombstone(ti.lg, rev.main, rev.sub)
}

// RangeSince returns all revisions from key(including) to end(excluding)
// at or after the given rev. The returned slice is sorted in the order
// of revision.
func (ti *treeIndex) RangeSince(key, end []byte, rev int64) []revision {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		keyi := ti.keyIndex(&keyIndex{key: key})
		if keyi == nil {
			return nil
		}
		return keyi.since(ti.lg, rev)
	}

	var revs []revision
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		revs = append(revs, ki.since(ti.lg, rev)...)
		return true
	})
	sort.Sort(revisions(revs))

	return revs
}

func (ti *treeIndex) Compact(rev int64) map[revision]struct{} {
	available := make(map[revision]struct{})
	ti.lg.Info("compact tree index", zap.Int64("revision", rev))
	ti.Lock()
	clone := ti.tree.Clone()
	ti.Unlock()

	clone.Ascend(func(item btree.Item) bool {
		keyi := item.(*keyIndex)
		// Lock is needed here to prevent modification to the keyIndex while
		// compaction is going on or revision added to empty before deletion
		ti.Lock()
		keyi.compact(ti.lg, rev, available)
		if keyi.isEmpty() {
			item := ti.tree.Delete(keyi)
			if item == nil {
				ti.lg.Panic("failed to delete during compaction")
			}
		}
		ti.Unlock()
		return true
	})
	return available
}

// Keep finds all revisions to be kept for a Compaction at the given rev.
func (ti *treeIndex) Keep(rev int64) map[revision]struct{} {
	available := make(map[revision]struct{})
	ti.RLock()
	defer ti.RUnlock()
	ti.tree.Ascend(func(i btree.Item) bool {
		keyi := i.(*keyIndex)
		keyi.keep(rev, available)
		return true
	})
	return available
}

func (ti *treeIndex) Equal(bi index) bool {
	b := bi.(*treeIndex)

	if ti.tree.Len() != b.tree.Len() {
		return false
	}

	equal := true

	ti.tree.Ascend(func(item btree.Item) bool {
		var aki, bki *keyIndex
		var ok bool
		if aki, ok = item.(*keyIndex); !ok {
			return false
		}
		if bki, ok = b.tree.Get(item).(*keyIndex); !ok {
			return false
		}
		if !aki.equal(bki) {
			equal = false
			return false
		}
		return true
	})

	return equal
}

func (ti *treeIndex) Insert(ki *keyIndex) {
	ti.Lock()
	defer ti.Unlock()
	ti.tree.ReplaceOrInsert(ki)
}
