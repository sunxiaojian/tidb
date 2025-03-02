// Copyright 2023 PingCAP, Inc.
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

package lru

import (
	"container/list"
	"math"
	"sync"

	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/statistics/handle/cache/internal"
	"github.com/pingcap/tidb/statistics/handle/metrics"
)

// StatsInnerCache is the LRU cache for statistics.
type StatsInnerCache struct {
	elements map[int64]*lruMapElement
	// lru maintains item lru cache
	lru *innerItemLruCache
	sync.RWMutex
}

// NewStatsLruCache creates a new LRU cache for statistics.
func NewStatsLruCache(c int64) *StatsInnerCache {
	s := &StatsInnerCache{
		elements: make(map[int64]*lruMapElement),
		lru:      newInnerLruCache(c),
	}
	s.lru.onEvict = s.onEvict
	return s
}

type innerItemLruCache struct {
	// elements maintains tableID -> isIndex -> columnID/indexID -> *lruCacheItem
	elements     map[int64]map[bool]map[int64]*list.Element
	cache        *list.List
	onEvict      func(tblID int64)
	capacity     int64
	trackingCost int64
}

func newInnerLruCache(c int64) *innerItemLruCache {
	if c < 1 {
		c = math.MaxInt64
	}
	metrics.CapacityGauge.Set(float64(c))
	return &innerItemLruCache{
		capacity: c,
		cache:    list.New(),
		elements: make(map[int64]map[bool]map[int64]*list.Element, 0),
	}
}

type lruCacheItem struct {
	innerItem     statistics.TableCacheItem
	innerMemUsage statistics.CacheItemMemoryUsage
	tblID         int64
	id            int64
	isIndex       bool
}

type lruMapElement struct {
	tbl         *statistics.Table
	tblMemUsage *statistics.TableMemoryUsage
}

func (l *lruMapElement) copy() *lruMapElement {
	return &lruMapElement{
		tbl:         l.tbl,
		tblMemUsage: l.tblMemUsage,
	}
}

// getByQuery implements statsCacheInner
func (s *StatsInnerCache) getByQuery(tblID int64) (*statistics.Table, bool) {
	s.Lock()
	defer s.Unlock()
	element, ok := s.elements[tblID]
	if !ok {
		return nil, false
	}
	// move index element
	for idxID := range element.tbl.Indices {
		s.lru.get(tblID, idxID, true)
	}
	// move column element
	for colID := range element.tbl.Columns {
		s.lru.get(tblID, colID, false)
	}
	return element.tbl, true
}

// Get implements statsCacheInner
func (s *StatsInnerCache) Get(tblID int64, moveLRUFront bool) (*statistics.Table, bool) {
	if moveLRUFront {
		return s.getByQuery(tblID)
	}
	s.RLock()
	defer s.RUnlock()
	element, ok := s.elements[tblID]
	if !ok {
		return nil, false
	}
	return element.tbl, true
}

// Put implements statsCacheInner
func (s *StatsInnerCache) Put(tblID int64, tbl *statistics.Table, moveLRUFront bool) {
	s.Lock()
	defer s.Unlock()
	s.put(tblID, tbl, tbl.MemoryUsage(), moveLRUFront)
}

func (s *StatsInnerCache) put(tblID int64, tbl *statistics.Table, tblMemUsage *statistics.TableMemoryUsage, needMove bool) {
	element, exist := s.elements[tblID]
	if exist {
		s.updateColumns(tblID, tbl, tblMemUsage, needMove)
		s.updateIndices(tblID, tbl, tblMemUsage, needMove)
		// idx mem usage might be changed before, thus we recalculate the tblMem usage
		element.tbl = tbl
		element.tblMemUsage = tbl.MemoryUsage()
		return
	}
	s.updateColumns(tblID, tbl, tblMemUsage, needMove)
	s.updateIndices(tblID, tbl, tblMemUsage, needMove)
	// mem usage might be changed due to evict, thus we recalculate the tblMem usage
	s.elements[tblID] = &lruMapElement{
		tbl:         tbl,
		tblMemUsage: tbl.MemoryUsage(),
	}
}

func (s *StatsInnerCache) updateIndices(tblID int64, tbl *statistics.Table, tblMemUsage *statistics.TableMemoryUsage, needMove bool) {
	_, exist := s.elements[tblID]
	if exist {
		oldIdxs := s.lru.elements[tblID][true]
		deletedIdx := make([]int64, 0)
		for oldIdxID := range oldIdxs {
			_, exist := tbl.Indices[oldIdxID]
			if !exist {
				deletedIdx = append(deletedIdx, oldIdxID)
			}
		}
		for _, idxID := range deletedIdx {
			s.lru.del(tblID, idxID, true)
		}
	}
	for idxID, idx := range tbl.Indices {
		idxMem := tblMemUsage.IndicesMemUsage[idxID]
		s.lru.put(tblID, idxID, true, idx, idxMem, true, needMove)
	}
}

func (s *StatsInnerCache) updateColumns(tblID int64, tbl *statistics.Table, tblMemUsage *statistics.TableMemoryUsage, needMove bool) {
	_, exist := s.elements[tblID]
	if exist {
		oldCols := s.lru.elements[tblID][false]
		deletedCol := make([]int64, 0)
		for oldColID := range oldCols {
			_, exist := tbl.Columns[oldColID]
			if !exist {
				deletedCol = append(deletedCol, oldColID)
			}
		}
		for _, colID := range deletedCol {
			s.lru.del(tblID, colID, false)
		}
	}
	for colID, col := range tbl.Columns {
		colMem := tblMemUsage.ColumnsMemUsage[colID]
		s.lru.put(tblID, colID, false, col, colMem, true, needMove)
	}
}

// Del implements statsCacheInner
func (s *StatsInnerCache) Del(tblID int64) {
	s.Lock()
	defer s.Unlock()
	element, exist := s.elements[tblID]
	if !exist {
		return
	}
	// remove indices
	for idxID := range element.tbl.Indices {
		s.lru.del(tblID, idxID, true)
	}
	// remove columns
	for colID := range element.tbl.Columns {
		s.lru.del(tblID, colID, false)
	}
	delete(s.elements, tblID)
}

// Cost implements statsCacheInner
func (s *StatsInnerCache) Cost() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.lru.trackingCost
}

// TotalCost returns the total memory usage of all tables.
func (s *StatsInnerCache) TotalCost() int64 {
	s.Lock()
	defer s.Unlock()
	totalCost := int64(0)
	for tblID, ele := range s.elements {
		s.freshTableCost(tblID, ele)
		totalCost += ele.tblMemUsage.TotalMemUsage
	}
	return totalCost
}

// Keys implements statsCacheInner
func (s *StatsInnerCache) Keys() []int64 {
	s.RLock()
	defer s.RUnlock()
	r := make([]int64, 0, len(s.elements))
	for tblID := range s.elements {
		r = append(r, tblID)
	}
	return r
}

// Values implements statsCacheInner
func (s *StatsInnerCache) Values() []*statistics.Table {
	s.RLock()
	defer s.RUnlock()
	r := make([]*statistics.Table, 0, len(s.elements))
	for _, v := range s.elements {
		r = append(r, v.tbl)
	}
	return r
}

// Len implements statsCacheInner
func (s *StatsInnerCache) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.elements)
}

// Copy implements statsCacheInner
func (s *StatsInnerCache) Copy() internal.StatsCacheInner {
	s.RLock()
	defer s.RUnlock()
	newCache := NewStatsLruCache(s.lru.capacity)
	newCache.lru = s.lru.copy()
	for tblID, element := range s.elements {
		newCache.elements[tblID] = element.copy()
	}
	newCache.lru.onEvict = newCache.onEvict
	return newCache
}

// SetCapacity implements statsCacheInner
func (s *StatsInnerCache) SetCapacity(c int64) {
	s.Lock()
	defer s.Unlock()
	s.lru.setCapacity(c)
}

// Front implements statsCacheInner
func (s *StatsInnerCache) Front() int64 {
	s.RLock()
	defer s.RUnlock()
	ele := s.lru.cache.Front()
	if ele == nil {
		return 0
	}
	return s.lru.cache.Front().Value.(*lruCacheItem).tblID
}

func (s *StatsInnerCache) onEvict(tblID int64) {
	element, exist := s.elements[tblID]
	if !exist {
		return
	}
	element.tblMemUsage = element.tbl.MemoryUsage()
}

func (s *StatsInnerCache) freshTableCost(tblID int64, element *lruMapElement) {
	element.tblMemUsage = element.tbl.MemoryUsage()
	s.put(tblID, element.tbl, element.tblMemUsage, false)
}

func (s *StatsInnerCache) capacity() int64 {
	return s.lru.capacity
}

func (c *innerItemLruCache) get(tblID, id int64, isIndex bool) (*lruCacheItem, bool) {
	v, ok := c.elements[tblID]
	if !ok {
		metrics.MissCounter.Inc()
		return nil, false
	}
	isIndexSet, ok := v[isIndex]
	if !ok {
		metrics.MissCounter.Inc()
		return nil, false
	}
	ele, ok := isIndexSet[id]
	if !ok {
		metrics.MissCounter.Inc()
		return nil, false
	}
	metrics.HitCounter.Inc()
	c.cache.MoveToFront(ele)
	return ele.Value.(*lruCacheItem), true
}

func (c *innerItemLruCache) del(tblID, id int64, isIndex bool) {
	v, ok := c.elements[tblID]
	if !ok {
		return
	}
	isindexSet, ok := v[isIndex]
	if !ok {
		return
	}
	ele, ok := isindexSet[id]
	if !ok {
		return
	}
	metrics.DelCounter.Inc()
	memUsage := c.elements[tblID][isIndex][id].Value.(*lruCacheItem).innerMemUsage
	delete(c.elements[tblID][isIndex], id)
	c.cache.Remove(ele)
	if isIndex {
		c.calculateCost(&statistics.IndexMemUsage{}, memUsage)
	} else {
		c.calculateCost(&statistics.ColumnMemUsage{}, memUsage)
	}
}

func (c *innerItemLruCache) put(tblID, id int64, isIndex bool, item statistics.TableCacheItem, itemMem statistics.CacheItemMemoryUsage,
	needEvict, needMove bool) {
	defer func() {
		metrics.UpdateCounter.Inc()
		if needEvict {
			c.evictIfNeeded()
		}
	}()
	if itemMem.TrackingMemUsage() < 1 {
		return
	}
	isIndexSet, ok := c.elements[tblID]
	if !ok {
		c.elements[tblID] = make(map[bool]map[int64]*list.Element)
		isIndexSet = c.elements[tblID]
	}
	v, ok := isIndexSet[isIndex]
	if !ok {
		c.elements[tblID][isIndex] = make(map[int64]*list.Element)
		v = c.elements[tblID][isIndex]
	}
	element, exist := v[id]
	if exist {
		oldItem := element.Value.(*lruCacheItem)
		oldMemUsage := oldItem.innerMemUsage
		oldItem.innerItem = item
		oldItem.innerMemUsage = itemMem
		c.calculateCost(itemMem, oldMemUsage)
		if needMove {
			c.cache.MoveToFront(element)
		}
		return
	}
	newItem := &lruCacheItem{
		tblID:         tblID,
		id:            id,
		innerItem:     item,
		innerMemUsage: itemMem,
		isIndex:       isIndex,
	}
	newElement := c.cache.PushFront(newItem)
	v[id] = newElement
	if isIndex {
		c.calculateCost(itemMem, &statistics.IndexMemUsage{})
	} else {
		c.calculateCost(itemMem, &statistics.ColumnMemUsage{})
	}
}

func (c *innerItemLruCache) evictIfNeeded() {
	curr := c.cache.Back()
	for c.trackingCost > c.capacity && curr != nil {
		metrics.EvictCounter.Inc()
		prev := curr.Prev()
		item := curr.Value.(*lruCacheItem)
		oldMem := item.innerMemUsage
		statistics.DropEvicted(item.innerItem)
		newMem := item.innerItem.MemoryUsage()
		c.calculateCost(newMem, oldMem)
		if newMem.TrackingMemUsage() == 0 || item.innerItem.IsAllEvicted() {
			// remove from lru
			c.cache.Remove(curr)
			delete(c.elements[item.tblID][item.isIndex], item.id)
		} else {
			c.cache.MoveToFront(curr)
			item.innerMemUsage = newMem
		}
		if c.onEvict != nil {
			c.onEvict(item.tblID)
		}
		curr = prev
	}
}

func (c *innerItemLruCache) calculateCost(newUsage, oldUsage statistics.CacheItemMemoryUsage) {
	c.trackingCost += newUsage.TrackingMemUsage() - oldUsage.TrackingMemUsage()
}

func (c *innerItemLruCache) copy() *innerItemLruCache {
	newLRU := newInnerLruCache(c.capacity)
	curr := c.cache.Back()
	for curr != nil {
		item := curr.Value.(*lruCacheItem)
		newLRU.put(item.tblID, item.id, item.isIndex, item.innerItem, item.innerMemUsage, false, false)
		curr = curr.Prev()
	}
	return newLRU
}

func (c *innerItemLruCache) setCapacity(capacity int64) {
	if capacity < 1 {
		capacity = math.MaxInt64
	}
	c.capacity = capacity
	metrics.CapacityGauge.Set(float64(c.capacity))
	c.evictIfNeeded()
}
