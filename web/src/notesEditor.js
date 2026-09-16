// web/src/notesEditor.js
// Notes mode (-notes): an editable Markdown surface that autosaves on every
// keystroke. Separate from the read-only virtualized code viewer (renderer.js)
// on purpose — that renderer is built around static, server-highlighted chunks,
// not live typing/caret editing. See docs/internals/notes-mode.md.
import { $, S, doc_, debounce, apiPostBody } from './state.js';
import { drawTabs, drawCrumbs } from './tabs.js';
import { updateStatus, setStatusNote } from './status.js';
import { pushHistory } from './history.js';
import { previewing, syncPreview } from './markdown.js';

const notePanel = $('#notesedit');
const noteText = $('#notesedit-text');
const noteStatusEl = $('#notesedit-status');

export function isNoteDoc(d) {
  return !!d && d.kind === 'note';
}

// Opens (or focuses, if already open) a Markdown file as an editable note tab.
// Unlike openFile()'s windowed /api/file, this loads the whole raw file at once
// via /api/raw — notes are small text files, no chunking needed.
export async function openNote(path, opts = {}) {
  const { push = true } = opts;
  let idx = S.tabs.findIndex(t => t.path === path);
  if (idx < 0) {
    let text;
    try {
      const r = await fetch('/api/raw?path=' + encodeURIComponent(path));
      if (!r.ok) throw new Error('HTTP ' + r.status);
      text = await r.text();
    } catch (e) {
      setStatusNote(path + ': ' + e.message);
      return;
    }
    const d = {
      path, name: path.split('/').pop(), kind: 'note', text, markdown: true,
      preview: false, // notes open editable; Alt+M / the status bar toggle switches to rendered preview
      size: text.length,
      dirty: false, saveSeq: 0, scrollTop: 0, selStart: 0, selEnd: 0,
    };
    S.tabs.push(d);
    idx = S.tabs.length - 1;
  }
  const prev = doc_();
  if (prev && prev !== S.tabs[idx]) flushIfDirty(prev);
  S.active = idx;
  $('#empty').hidden = true;
  syncPreview();
  syncNotesEditor();
  drawTabs();
  drawCrumbs();
  updateStatus();
  if (push) pushHistory(path, 1);
}

// Shows or hides #notesedit to match the active tab; call wherever
// syncPreview()/syncDiffView() already run (tab open/switch/close). Hidden
// while the doc's rendered preview is showing instead (see markdown.js).
export function syncNotesEditor() {
  const d = doc_();
  const want = isNoteDoc(d) && !previewing(d) ? d : null;
  if (notePanel) notePanel.hidden = !want;
  if (!want) return;
  if (noteText.dataset.path !== want.path) {
    noteText.value = want.text;
    noteText.dataset.path = want.path;
    noteText.scrollTop = want.scrollTop || 0;
    if (want.selStart != null) noteText.setSelectionRange(want.selStart, want.selEnd);
  }
  setSaveStatus(want.dirty ? 'saving' : 'saved');
}

const scheduleSave = debounce(d => saveNote(d), 500);

async function saveNote(d) {
  const mySeq = ++d.saveSeq;
  try {
    const res = await apiPostBody('/api/save', { path: d.path }, d.text);
    if (mySeq !== d.saveSeq) return; // superseded by a newer keystroke's save
    d.dirty = false;
    d.mtime = res.mtime;
    if (doc_() === d) setSaveStatus('saved');
  } catch (e) {
    if (doc_() === d) setSaveStatus('error', e.message);
  }
}

// Immediate (non-debounced) save, used before navigating away from a dirty
// note (or switching it into preview) so the last <500ms of typing isn't lost
// or shown stale. Returns the save's promise so a caller can await it.
export function flushIfDirty(d) {
  if (isNoteDoc(d) && d.dirty) return saveNote(d);
}

function setSaveStatus(state, detail) {
  if (!noteStatusEl) return;
  noteStatusEl.textContent = state === 'saved' ? 'Saved' : state === 'saving' ? 'Saving…' : 'Save failed: ' + detail;
}

export function initNotesEditor() {
  if (!noteText) return;
  noteText.addEventListener('input', () => {
    const d = doc_();
    if (!isNoteDoc(d)) return;
    d.text = noteText.value;
    d.size = d.text.length;
    d.dirty = true;
    setSaveStatus('saving');
    updateStatus();
    scheduleSave(d);
  });
  noteText.addEventListener('scroll', () => {
    const d = doc_();
    if (isNoteDoc(d)) d.scrollTop = noteText.scrollTop;
  });
  noteText.addEventListener('beforeinput', () => {
    const d = doc_();
    if (isNoteDoc(d)) { d.selStart = noteText.selectionStart; d.selEnd = noteText.selectionEnd; }
  });
  window.addEventListener('beforeunload', () => {
    const d = doc_();
    if (isNoteDoc(d) && d.dirty) {
      navigator.sendBeacon('/api/save?path=' + encodeURIComponent(d.path), d.text);
    }
  });
}
