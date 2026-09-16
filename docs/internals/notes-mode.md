# Notes Mode (`-notes`)

This document describes `px0 -notes <dir>`, an opt-in mode that turns px0 into an Obsidian-style Markdown notes editor: the file tree shows only Markdown files, opening one gives an editable surface, and edits autosave to disk on every keystroke. It is the one sanctioned exception to px0's [read-only design](../agents/README.md) — everything below exists to keep that exception narrow and auditable.

Default `px0 <dir>` (no flag) is completely unaffected: every code path in this document is gated behind `Index.notes` / `Server.ix.NotesMode()` being true.

## 1. Markdown-only tree: why it's a separate build path

[`index.go`](../../index.go)'s default `Build()` (see [indexing-and-ignore.md](indexing-and-ignore.md)) streams top-down: a directory's `Node` is emitted and published to `ix.children` before its subdirectories are even read. That works for the default tree, where every directory is shown regardless of contents.

Notes mode needs the opposite: a directory should be **pruned** if nothing anywhere beneath it is a `.md`/`.markdown` file. Whether to keep a directory is a bottom-up fact — it can't be known until its whole subtree has been walked — so `buildNotes()` (also in `index.go`) is a distinct, single-pass **recursive** walk that returns each directory's kept children before its parent decides whether to keep the directory itself. It reuses `isMarkdown()` ([`markdown.go`](../../markdown.go)) — the same test the existing `/api/markdown` preview endpoint uses — so notes mode and Markdown preview never disagree about what counts as a note, and it reuses `readGitignore`/`ignoreSet`/`gitStatus`/`sortNodes` from the default path unchanged.

Two behavioral differences from default mode, both intentional:
- **Gitignored entries are dropped outright**, not listed dimmed. The default mode lists an ignored directory with `Ignored: true` because its contents are knowable on demand (`listIgnored`); notes mode never descends into it, so whether it hides a Markdown file is unknowable, and a notes vault has no use for that clutter regardless.
- **A single-goroutine walk**, not `Build()`'s `NumCPU()*4` worker pool. Notes vaults are orders of magnitude smaller than code repositories, so the pruning logic stays simple to reason about rather than parallelized.

`Index.Children()`, `underIgnoredLocked()`, and `listIgnored()` need no changes: in notes mode no `Node` ever has `Ignored: true`, so the ignored-listing fallback path is simply never exercised.

## 2. `POST /api/save`: the read-only exception, narrowly gated

[`server.go`](../../server.go)'s `handleSave` is reachable only when **all** of these hold:
1. **`localPost(w, r)`** — the same same-origin + localhost/IP check `/api/lsp/install` uses, blocking CSRF and DNS rebinding (see [architecture.md](architecture.md) §5).
2. **`s.ix.NotesMode()`** — false on every default-mode server, so the endpoint 403s outright outside `-notes`.
3. **`s.safePath(path)`** — not `resolvePath`, which additionally allows LSP-resolved absolute paths outside the workspace root. A write endpoint must never carry that escape hatch.
4. **`isMarkdown(rel)`** — even inside a notes-mode workspace, only `.md`/`.markdown` files can be overwritten.

The body is capped at 8 MiB (`maxNoteBytes`) via `http.MaxBytesReader` and written with `os.WriteFile(abs, body, 0644)`, replacing the file's full contents (the client always sends the complete note text, not a diff).

## 3. The reload-clobber hazard, and how it's avoided

[`file-reload-and-updates.md`](file-reload-and-updates.md) documents `reloadOpenTabs()`: after a reindex, open tabs are quietly refetched from the server's highlight cache, keyed on `abs|mtime|size`. An autosave write changes both mtime and size — so without precaution, a reindex firing while the user is mid-keystroke in a note would refetch and overwrite their in-memory buffer with server-round-tripped content, racing the debounced autosave.

The fix is a one-line filter in `reloadOpenTabs()`'s target list: `S.tabs.filter(t => !isNoteDoc(t))`. Note tabs (`doc.kind === 'note'`, from [`web/src/notesEditor.js`](../../web/src/notesEditor.js)) are simply never included in the refetch batch. While a note is open, its in-memory `text` buffer plus the debounced `POST /api/save` calls are the sole source of truth for that file — nothing else is allowed to rewrite it out from under the user. The same tabs.js file also guards the scroll-position bookkeeping around `switchTab()`/`closeTab()`/`reloadOpenTabs()` (`vp.scrollTop` tracking) to skip note tabs, since that tracking is meaningless for a plain `<textarea>` outside the virtualized `#viewport`.

## 4. Editor surface: why not the existing code viewer

[`renderer.js`](../../web/src/renderer.js) is a read-only virtualized viewer over server-highlighted, windowed chunks (`/api/file`) — built for scroll performance on huge files, not for caret placement or live text mutation. Rather than retrofit it, notes mode mounts a separate `#notesedit` panel (a plain `<textarea id="notesedit-text">`) as a sibling of `#mdview` inside `#editor`, loaded via `/api/raw?path=...` (the whole file at once — notes are small, no windowing needed). `web/src/tabs.js`'s `openFile()` is the single routing choke point: `if (S.meta?.notesMode && isMarkdownPath(path)) return openNote(path, opts)`, covering every existing caller (tree clicks, quick-open, history navigation, the initial `?path=` URL param) without changes elsewhere.

## 5. Related documentation

- [architecture.md](architecture.md) — HTTP endpoint catalog and the `safePath`/`resolvePath` sandbox `handleSave` builds on.
- [file-reload-and-updates.md](file-reload-and-updates.md) — `reloadOpenTabs()`, whose target-filtering this mode depends on.
- [indexing-and-ignore.md](indexing-and-ignore.md) — the default `Build()`/`walk()` that `buildNotes()` forks from.
- [markdown.md](markdown.md) — the read-only Markdown preview pipeline (`isMarkdown`, goldmark rendering), unrelated to but reused by notes mode.
