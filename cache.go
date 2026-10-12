package qcow2

import (
	"cmp"
	"container/list"
	"slices"
)

// tableCache is a small LRU cache of metadata clusters (L2 tables and
// refcount blocks) keyed by host offset. Modified tables are marked dirty
// and only written by Image.flush, in an order that keeps the image
// consistent on disk; dirty entries are never evicted.
type tableCache struct {
	cap   int
	ll    *list.List
	m     map[int64]*list.Element
	dirty int
}

type tableCacheEntry struct {
	off   int64
	buf   []byte
	dirty bool
}

func newTableCache(capacity int) *tableCache {
	if capacity < 4 {
		capacity = 4
	}
	return &tableCache{cap: capacity, ll: list.New(), m: make(map[int64]*list.Element)}
}

func (c *tableCache) get(off int64) []byte {
	if e, ok := c.m[off]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*tableCacheEntry).buf
	}
	return nil
}

// put inserts or replaces a table.
func (c *tableCache) put(off int64, buf []byte, dirty bool) {
	if e, ok := c.m[off]; ok {
		ent := e.Value.(*tableCacheEntry)
		ent.buf = buf
		c.setDirty(ent, ent.dirty || dirty)
		c.ll.MoveToFront(e)
		return
	}
	ent := &tableCacheEntry{off: off, buf: buf}
	c.m[off] = c.ll.PushFront(ent)
	c.setDirty(ent, dirty)
	for e := c.ll.Back(); c.ll.Len() > c.cap && e != nil; {
		prev := e.Prev()
		if old := e.Value.(*tableCacheEntry); !old.dirty {
			c.ll.Remove(e)
			delete(c.m, old.off)
		}
		e = prev
	}
}

// markDirty records that buf, the table at off, has been modified.
func (c *tableCache) markDirty(off int64, buf []byte) {
	c.put(off, buf, true)
}

func (c *tableCache) setDirty(ent *tableCacheEntry, dirty bool) {
	if ent.dirty != dirty {
		if dirty {
			c.dirty++
		} else {
			c.dirty--
		}
		ent.dirty = dirty
	}
}

// dirtyEntries returns the dirty tables sorted by offset.
func (c *tableCache) dirtyEntries() []*tableCacheEntry {
	var es []*tableCacheEntry
	for _, e := range c.m {
		if ent := e.Value.(*tableCacheEntry); ent.dirty {
			es = append(es, ent)
		}
	}
	slices.SortFunc(es, func(a, b *tableCacheEntry) int { return cmp.Compare(a.off, b.off) })
	return es
}

func (c *tableCache) remove(off int64) {
	if e, ok := c.m[off]; ok {
		c.setDirty(e.Value.(*tableCacheEntry), false)
		c.ll.Remove(e)
		delete(c.m, off)
	}
}
