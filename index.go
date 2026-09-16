package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type FileEntry struct {
	Path      string `json:"path"` // slash-separated, relative to root
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	lower     string // cached lowercase Path for matching
	nameStart int    // index in Path where the basename begins
}

type Node struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Ignored bool   `json:"ignored,omitempty"` // matched by .gitignore: listed, never indexed
	Status  string `json:"status,omitempty"`  // git working-tree status: M/A/D/?/R/C/U
	Dirty   bool   `json:"dirty,omitempty"`   // folder: contains a git-changed descendant
}

// vcsDirs are version control internals. Unlike other ignored entries they are
// not even listed: nobody reads them, and .git is present in nearly every repo.
var vcsDirs = map[string]bool{".git": true, ".hg": true, ".svn": true}

type Index struct {
	root  string
	notes bool // restrict the tree to Markdown files/folders (notes mode)

	mu       sync.RWMutex
	files    []FileEntry
	children map[string][]Node
	builtAt  time.Time
	buildMS  int64
	readyCh  chan struct{}
}

func NewIndex(root string, notes bool) *Index {
	return &Index{root: root, notes: notes, children: map[string][]Node{}, readyCh: make(chan struct{})}
}

func (ix *Index) Root() string { return ix.root }

func (ix *Index) NotesMode() bool { return ix.notes }

func (ix *Index) Ready() bool {
	select {
	case <-ix.readyCh:
		return true
	default:
		return false
	}
}

func (ix *Index) WaitReady(ctx context.Context) error {
	select {
	case <-ix.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (ix *Index) Stats() (files int, builtAt time.Time, ms int64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.files), ix.builtAt, ix.buildMS
}

func (ix *Index) Files() []FileEntry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.files
}

// Children lists a directory for the tree. Ignored directories are never walked,
// so their contents are read from disk on demand, all marked ignored: git cannot
// re-include anything beneath an excluded directory either.
func (ix *Index) Children(dir string) ([]Node, bool) {
	ix.mu.RLock()
	c, ok := ix.children[dir]
	under := !ok && ix.underIgnoredLocked(dir)
	ix.mu.RUnlock()
	if ok || !under {
		return c, ok
	}
	return ix.listIgnored(dir)
}

// underIgnoredLocked reports whether dir sits inside a directory the walk
// listed as ignored. The caller holds ix.mu.
func (ix *Index) underIgnoredLocked(dir string) bool {
	if dir == "" || strings.ContainsRune(dir, '\\') {
		return false
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false // never let a crafted path climb out of the root
		}
	}
	// The nearest ancestor the walk visited decides: its entry for the next
	// segment down must be an ignored directory.
	for p := dir; p != ""; {
		parent := ""
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent = p[:i]
		}
		if kids, ok := ix.children[parent]; ok {
			name := strings.TrimPrefix(p[len(parent):], "/")
			for _, k := range kids {
				if k.Name == name {
					return k.Dir && k.Ignored
				}
			}
			return false
		}
		p = parent
	}
	return false
}

func (ix *Index) listIgnored(dir string) ([]Node, bool) {
	ents, err := os.ReadDir(filepath.Join(ix.root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, false
	}
	kids := make([]Node, 0, len(ents))
	for _, e := range ents {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		kids = append(kids, Node{Name: e.Name(), Path: dir + "/" + e.Name(), Dir: e.IsDir(), Ignored: true})
	}
	sortNodes(kids)
	return kids, true
}

// sortNodes orders a listing directories first, then case-insensitively by name.
func sortNodes(kids []Node) {
	sort.Slice(kids, func(i, j int) bool {
		if kids[i].Dir != kids[j].Dir {
			return kids[i].Dir
		}
		return strings.ToLower(kids[i].Name) < strings.ToLower(kids[j].Name)
	})
}

// Build walks the tree once, honouring .gitignore at every level, and
// materialises both the flat file list (for fuzzy find and search) and the
// directory map (for the tree view). Root entries are published immediately so
// the frontend can display the file tree without waiting for the full repo scan.
func (ix *Index) Build() {
	if ix.notes {
		ix.buildNotes()
		return
	}
	start := time.Now()
	root := newIgnoreSet(nil)
	root = root.child(readGitignore(ix.root, ""))

	var (
		mu       sync.Mutex
		files    []FileEntry
		children = map[string][]Node{}
		wg       sync.WaitGroup
		sem      = make(chan struct{}, runtime.NumCPU()*4)
	)

	// git status only needs the repo root, not the walk result, so run it
	// concurrently with the walk instead of serially after it — on large repos
	// the ~80ms subprocess overlaps the tree scan rather than adding to it.
	gsCh := make(chan map[string]string, 1)
	go func() { gsCh <- gitStatus(ix.root) }()

	var walk func(abs, rel string, ig *ignoreSet)
	walk = func(abs, rel string, ig *ignoreSet) {
		defer wg.Done()
		ents, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		if rel != "" {
			if extra := readGitignore(abs, rel); len(extra) > 0 {
				ig = ig.child(extra)
			}
		}
		kids := make([]Node, 0, len(ents))
		var subdirs []struct {
			abs, rel string
		}
		for _, e := range ents {
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			isDir := e.IsDir()
			// Follow nothing through symlinks; cycles are not worth the risk.
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if ig.match(childRel, isDir) {
				// Listed so the tree can show it dimmed, but never walked or
				// indexed, so search and quick open stay out of it.
				if !(isDir && vcsDirs[name]) {
					kids = append(kids, Node{Name: name, Path: childRel, Dir: isDir, Ignored: true})
				}
				continue
			}
			if isDir {
				kids = append(kids, Node{Name: name, Path: childRel, Dir: true})
				subdirs = append(subdirs, struct{ abs, rel string }{filepath.Join(abs, name), childRel})
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			kids = append(kids, Node{Name: name, Path: childRel, Size: info.Size()})
			mu.Lock()
			files = append(files, FileEntry{
				Path: childRel, Name: name, Size: info.Size(),
				lower: strings.ToLower(childRel), nameStart: len(childRel) - len(name),
			})
			mu.Unlock()
		}
		sortNodes(kids)
		mu.Lock()
		children[rel] = kids
		mu.Unlock()

		// If this is the root directory, make it available to ix.Children("")
		// immediately so the browser UI can render the sidebar tree without delay.
		if rel == "" {
			ix.mu.Lock()
			if ix.children == nil {
				ix.children = map[string][]Node{}
			}
			ix.children[""] = kids
			ix.mu.Unlock()
		}

		for _, sd := range subdirs {
			wg.Add(1)
			select {
			case sem <- struct{}{}:
				go func(a, r string, g *ignoreSet) {
					defer func() { <-sem }()
					walk(a, r, g)
				}(sd.abs, sd.rel, ig)
			default:
				walk(sd.abs, sd.rel, ig) // pool saturated: recurse inline
			}
		}
	}

	wg.Add(1)
	walk(ix.root, "", root)
	wg.Wait()

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// Overlay git working-tree status onto file nodes (computed concurrently with
	// the walk above); nil when git is unavailable or off.
	gs := <-gsCh

	// Every ancestor directory of a changed file is dirty, so a collapsed folder
	// can badge without the frontend fetching its subtree.
	dirtyDirs := map[string]bool{}
	for p := range gs {
		for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p, '/') {
			p = p[:i]
			dirtyDirs[p] = true
		}
	}

	ix.mu.Lock()
	if gs != nil {
		for _, kids := range children {
			for i := range kids {
				if kids[i].Dir {
					kids[i].Dirty = dirtyDirs[kids[i].Path]
				} else if code, ok := gs[kids[i].Path]; ok {
					kids[i].Status = code
				}
			}
		}
	}
	ix.files, ix.children = files, children
	ix.builtAt, ix.buildMS = time.Now(), time.Since(start).Milliseconds()
	select {
	case <-ix.readyCh:
	default:
		close(ix.readyCh)
	}
	ix.mu.Unlock()
}

// buildNotes walks the tree once, like Build, but keeps only Markdown files and
// the directories that contain at least one of them anywhere in their subtree
// (Obsidian-style vault view). Whether a folder survives depends on its whole
// subtree, so this is necessarily bottom-up: a directory's kept children must be
// known before the directory itself can be kept, which the streaming top-down
// walk in Build cannot provide. Notes vaults are orders of magnitude smaller
// than code repos, so a single-goroutine recursive walk (rather than Build's
// worker-pool fan-out) keeps the pruning logic simple without a measurable cost.
//
// Entries matched by .gitignore are dropped outright rather than listed dimmed:
// their contents are never walked, so whether one hides a Markdown file is
// unknowable, and a notes vault has no use for non-note clutter either way.
func (ix *Index) buildNotes() {
	start := time.Now()
	root := newIgnoreSet(nil)
	root = root.child(readGitignore(ix.root, ""))

	type rawEntry struct {
		node     Node
		children []rawEntry
	}

	var walk func(abs, rel string, ig *ignoreSet) []rawEntry
	walk = func(abs, rel string, ig *ignoreSet) []rawEntry {
		ents, err := os.ReadDir(abs)
		if err != nil {
			return nil
		}
		if rel != "" {
			if extra := readGitignore(abs, rel); len(extra) > 0 {
				ig = ig.child(extra)
			}
		}
		var out []rawEntry
		for _, e := range ents {
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			isDir := e.IsDir()
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if ig.match(childRel, isDir) {
				continue
			}
			if isDir {
				kids := walk(filepath.Join(abs, name), childRel, ig)
				if len(kids) == 0 {
					continue // prune: no Markdown anywhere beneath it
				}
				out = append(out, rawEntry{node: Node{Name: name, Path: childRel, Dir: true}, children: kids})
				continue
			}
			if !isMarkdown(childRel) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, rawEntry{node: Node{Name: name, Path: childRel, Size: info.Size()}})
		}
		return out
	}
	top := walk(ix.root, "", root)

	var files []FileEntry
	children := map[string][]Node{}
	var flatten func(rel string, ents []rawEntry)
	flatten = func(rel string, ents []rawEntry) {
		kids := make([]Node, len(ents))
		for i, e := range ents {
			kids[i] = e.node
			if !e.node.Dir {
				files = append(files, FileEntry{
					Path: e.node.Path, Name: e.node.Name, Size: e.node.Size,
					lower: strings.ToLower(e.node.Path), nameStart: len(e.node.Path) - len(e.node.Name),
				})
			}
		}
		sortNodes(kids)
		children[rel] = kids
		for _, e := range ents {
			if e.node.Dir {
				flatten(e.node.Path, e.children)
			}
		}
	}
	flatten("", top)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	gs := gitStatus(ix.root)
	dirtyDirs := map[string]bool{}
	for p := range gs {
		for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p, '/') {
			p = p[:i]
			dirtyDirs[p] = true
		}
	}
	if gs != nil {
		for _, kids := range children {
			for i := range kids {
				if kids[i].Dir {
					kids[i].Dirty = dirtyDirs[kids[i].Path]
				} else if code, ok := gs[kids[i].Path]; ok {
					kids[i].Status = code
				}
			}
		}
	}

	ix.mu.Lock()
	ix.files, ix.children = files, children
	ix.builtAt, ix.buildMS = time.Now(), time.Since(start).Milliseconds()
	select {
	case <-ix.readyCh:
	default:
		close(ix.readyCh)
	}
	ix.mu.Unlock()
}
