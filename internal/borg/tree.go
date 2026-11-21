package borg

import (
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tree is the directory structure of an archive, used to browse it.
type Tree struct {
	items    map[string]Item
	children map[string][]string
}

func newTree() *Tree {
	return &Tree{items: map[string]Item{}, children: map[string][]string{}}
}

func cleanPath(p string) string {
	p = strings.Trim(path.Clean("/"+p), "/")
	return p
}

func parentOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// add inserts an item. Borg 1.x does not always store the parent directories
// of backed up paths, so missing parents are added as directories.
func (t *Tree) add(it Item) {
	it.Path = cleanPath(it.Path)
	if it.Path == "" {
		return
	}
	if _, exists := t.items[it.Path]; !exists {
		t.children[parentOf(it.Path)] = append(t.children[parentOf(it.Path)], it.Path)
	}
	t.items[it.Path] = it
	for p := parentOf(it.Path); p != ""; p = parentOf(p) {
		if _, ok := t.items[p]; ok {
			break
		}
		t.items[p] = Item{Path: p, Name: path.Base(p), Type: "d"}
		t.children[parentOf(p)] = append(t.children[parentOf(p)], p)
	}
}

// finish sorts all directory listings: directories first, then by name.
func (t *Tree) finish() {
	for _, list := range t.children {
		sort.Slice(list, func(i, j int) bool {
			a, b := t.items[list[i]], t.items[list[j]]
			if a.IsDir() != b.IsDir() {
				return a.IsDir()
			}
			return a.Name < b.Name
		})
	}
}

// Len returns the number of items.
func (t *Tree) Len() int { return len(t.items) }

// Children returns the entries of a directory ("" is the root).
func (t *Tree) Children(dir string) []Item {
	paths := t.children[cleanPath(dir)]
	out := make([]Item, len(paths))
	for i, p := range paths {
		out[i] = t.items[p]
	}
	return out
}

// Below returns the items with the given paths and everything below them,
// each directory before its contents. Paths that are not in the tree and
// paths inside another given directory are skipped.
func (t *Tree) Below(paths []string) []Item {
	var out []Item
	seen := map[string]bool{}
	var walk func(p string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, t.items[p])
		for _, c := range t.children[p] {
			walk(c)
		}
	}
	for _, p := range paths {
		if it, ok := t.items[cleanPath(p)]; ok {
			walk(it.Path)
		}
	}
	return out
}

// Lookup returns the item with the given path.
func (t *Tree) Lookup(p string) (Item, bool) {
	it, ok := t.items[cleanPath(p)]
	return it, ok
}

// treeCache keeps the most recently used archive trees for a while, because
// listing a large archive with borg is slow.
type treeCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]*treeEntry
}

type treeEntry struct {
	ready   chan struct{}
	tree    *Tree
	err     error
	created time.Time
	used    time.Time
}

func newTreeCache(max int, ttl time.Duration) *treeCache {
	return &treeCache{max: max, ttl: ttl, entries: map[string]*treeEntry{}}
}

// get returns the cached tree or builds it; concurrent callers for the same
// key wait for a single build.
func (c *treeCache) get(key string, build func() (*Tree, error)) (*Tree, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && time.Since(e.created) > c.ttl {
		select {
		case <-e.ready:
			delete(c.entries, key)
			ok = false
		default:
		}
	}
	if !ok {
		e = &treeEntry{ready: make(chan struct{}), created: time.Now()}
		c.entries[key] = e
		c.evict()
		c.mu.Unlock()
		e.tree, e.err = build()
		close(e.ready)
		c.mu.Lock()
		if e.err != nil {
			delete(c.entries, key)
		}
	}
	e.used = time.Now()
	c.mu.Unlock()
	<-e.ready
	return e.tree, e.err
}

// evict removes the least recently used finished entries above the limit.
func (c *treeCache) evict() {
	for len(c.entries) > c.max {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			select {
			case <-e.ready:
			default:
				continue
			}
			if oldestKey == "" || e.used.Before(oldest) {
				oldestKey, oldest = k, e.used
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// dropRepo forgets the trees of a repository (after prune).
func (c *treeCache) dropRepo(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, location+"::") {
			delete(c.entries, k)
		}
	}
}
