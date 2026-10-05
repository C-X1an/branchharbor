package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store has one OS owner and serializes mutations. A successful mutation is
// published in memory only after its journal record has been synchronized.
type Store struct {
	mu                      sync.RWMutex
	dir                     string
	heads                   map[string]Head
	retries                 map[string]retry
	journal, lock           *os.File
	seq                     uint64
	prevHash                string
	journalBytes, diskBytes int64
	poisoned, closed        bool
	hook                    func(string) error // Test-only injection, never configured by a client.
}

func Open(dir string) (s *Store, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = ensureDir(dir); err != nil {
		return nil, err
	}
	s = &Store{dir: dir, heads: map[string]Head{}, retries: map[string]retry{}}
	s.lock, err = acquire(filepath.Join(dir, "LOCK"))
	if err != nil {
		return nil, err
	}
	owned := s
	defer func() {
		if err != nil {
			owned.Close()
		}
	}()
	format := filepath.Join(dir, "FORMAT")
	f, e := openPrivate(format, os.O_RDONLY)
	if e == nil {
		b, e := io.ReadAll(io.LimitReader(f, 128))
		f.Close()
		if e != nil {
			return nil, e
		}
		if !bytes.Equal(b, []byte("branchharbor/1\n")) {
			return nil, fail("invalid", "unsupported storage format")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	} else {
		// Only a known partial initialization can be resumed. Never adopt arbitrary
		// files in an unformatted directory.
		files, e := os.ReadDir(dir)
		if e != nil {
			return nil, e
		}
		for _, file := range files {
			switch file.Name() {
			case "LOCK", "objects", "journal", "auth.key":
			default:
				return nil, fail("invalid", "nonempty unformatted directory")
			}
		}
	}
	if err = ensureDir(filepath.Join(dir, "objects")); err != nil {
		return nil, err
	}
	for _, kind := range []string{"blob", "tree", "commit"} {
		if err = ensureDir(filepath.Join(dir, "objects", kind)); err != nil {
			return nil, err
		}
	}
	// Inspect the layout before any initialization writes.
	if s.diskBytes, err = s.countBytes(); err != nil {
		return nil, err
	}
	if s.diskBytes > MaxStoreBytes {
		return nil, fail("limit", "physical storage limit")
	}
	if _, e = os.Lstat(format); os.IsNotExist(e) {
		if err = s.writeAtomic(format, []byte("branchharbor/1\n"), "metadata"); err != nil {
			return nil, err
		}
	}
	s.journal, err = openPrivate(filepath.Join(dir, "journal"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		return nil, err
	}
	if err = s.recover(); err != nil {
		return nil, err
	}
	if s.diskBytes, err = s.countBytes(); err != nil {
		return nil, err
	}
	if s.seq == 0 {
		tree, err := s.putObject("tree", []byte("[]"))
		if err != nil {
			return nil, err
		}
		c := Commit{1, tree, []string{}, "system", "genesis"}
		b, _ := json.Marshal(c)
		id, err := s.putObject("commit", b)
		if err != nil {
			return nil, err
		}
		fp, _ := fingerprint("init", "system", struct{}{})
		if err = s.appendRecord("init", "main", "system", "genesis", fp, Head{}, Head{id, 1}); err != nil {
			return nil, err
		}
	}
	if _, ok := s.heads["main"]; !ok {
		return nil, fail("unavailable", "missing main reference")
	}
	if _, err = s.markReachable(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if s.journal != nil {
		errs = append(errs, s.journal.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	return errors.Join(errs...)
}
func (s *Store) check() error {
	if s.closed || s.poisoned {
		return fail("unavailable", "store requires reopen")
	}
	return nil
}
func (s *Store) Ready() bool { s.mu.RLock(); defer s.mu.RUnlock(); return !s.closed && !s.poisoned }
func (s *Store) Head(branch string) (Head, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return Head{}, err
	}
	return s.head(branch)
}
func (s *Store) head(branch string) (Head, error) {
	if !ValidBranch(branch) {
		return Head{}, fail("invalid", "invalid branch")
	}
	h, ok := s.heads[branch]
	if !ok {
		return Head{}, fail("not_found", "branch not found")
	}
	return h, nil
}
func (s *Store) Branches() (map[string]Head, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	out := map[string]Head{}
	for k, v := range s.heads {
		out[k] = v
	}
	return out, nil
}
func (s *Store) Snapshot(branch string) (Head, []Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return Head{}, nil, err
	}
	h, err := s.head(branch)
	if err != nil {
		return h, nil, err
	}
	entries, err := s.snapshot(h.Commit)
	return h, entries, err
}
func (s *Store) Read(branch, path string) (Head, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return Head{}, nil, err
	}
	if !ValidPath(path) {
		return Head{}, nil, fail("invalid", "invalid path")
	}
	h, err := s.head(branch)
	if err != nil {
		return h, nil, err
	}
	b, err := s.readAt(h.Commit, path)
	return h, b, err
}
func (s *Store) readAt(id, path string) ([]byte, error) {
	entries, err := s.snapshot(id)
	if err != nil {
		return nil, err
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Path >= path })
	if i == len(entries) || entries[i].Path != path {
		return nil, fail("not_found", "file not found")
	}
	b, err := s.readObject("blob", entries[i].Digest)
	if err == nil && int64(len(b)) != entries[i].Size {
		return nil, fail("unavailable", "blob size mismatch")
	}
	return b, err
}

// ReadAt is trusted in-process inspection, not exposed as a digest-based HTTP API.
func (s *Store) ReadAt(id, path string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	if !ValidPath(path) {
		return nil, fail("invalid", "invalid path")
	}
	return s.readAt(id, path)
}
func (s *Store) CommitInfo(id string) (Commit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return Commit{}, err
	}
	return s.commit(id)
}
func (s *Store) retry(actor, id, fp string) (Result, bool, error) {
	r, ok := s.retries[actor+"\x00"+id]
	if !ok {
		return Result{}, false, nil
	}
	if r.Fingerprint != fp {
		return Result{}, true, fail("idempotency_conflict", "request identity already used")
	}
	return Result{r.Head, true}, true, nil
}
func validMutation(actor, id string) error {
	if !ValidID(actor) || !ValidID(id) {
		return fail("invalid", "invalid actor or request identity")
	}
	return nil
}
func compareHead(current, expected Head) error {
	if current != expected {
		return fail("conflict", "stale branch head")
	}
	if current.Version == ^uint64(0) {
		return fail("limit", "branch version exhausted")
	}
	return nil
}
func mapTree(entries []Entry) map[string]Entry {
	m := make(map[string]Entry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m
}
func listTree(m map[string]Entry) []Entry {
	out := make([]Entry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
func (s *Store) publish(kind, branch, actor, id, fp string, old Head, entries []Entry, parents []string) (Result, error) {
	b, err := CanonicalTree(entries)
	if err != nil {
		return Result{}, err
	}
	tree, err := s.putObject("tree", b)
	if err != nil {
		return Result{}, err
	}
	b, err = json.Marshal(Commit{1, tree, parents, actor, id})
	if err != nil {
		return Result{}, err
	}
	cid, err := s.putObject("commit", b)
	if err != nil {
		return Result{}, err
	}
	next := Head{cid, old.Version + 1}
	if err = s.appendRecord(kind, branch, actor, id, fp, old, next); err != nil {
		return Result{}, err
	}
	return Result{next, false}, nil
}
func (s *Store) Commit(actor string, req CommitRequest) (Result, error) {
	if err := validMutation(actor, req.RequestID); err != nil {
		return Result{}, err
	}
	if !ValidBranch(req.Branch) {
		return Result{}, fail("invalid", "invalid branch")
	}
	if len(req.Puts)+len(req.Deletes) == 0 {
		return Result{}, fail("invalid", "empty transaction")
	}
	if len(req.Puts)+len(req.Deletes) > MaxChanges {
		return Result{}, fail("limit", "too many changes")
	}
	// Normalize a private copy: caller-owned slices must not be reordered.
	req.Deletes = append([]string{}, req.Deletes...)
	sort.Strings(req.Deletes)
	puts := make(map[string][]byte, len(req.Puts))
	for path, b := range req.Puts {
		if !ValidPath(path) || b == nil {
			return Result{}, fail("invalid", "invalid put")
		}
		if len(b) > MaxFileBytes {
			return Result{}, fail("limit", "file too large")
		}
		puts[path] = bytes.Clone(b)
	}
	req.Puts = puts
	for i, path := range req.Deletes {
		if !ValidPath(path) || i > 0 && req.Deletes[i-1] == path {
			return Result{}, fail("invalid", "invalid or duplicate delete")
		}
		if _, ok := req.Puts[path]; ok {
			return Result{}, fail("invalid", "put/delete overlap")
		}
	}
	fp, err := fingerprint("commit", actor, req)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.check(); err != nil {
		return Result{}, err
	}
	if r, found, err := s.retry(actor, req.RequestID, fp); found {
		return r, err
	}
	old, err := s.head(req.Branch)
	if err != nil {
		return Result{}, err
	}
	if err = compareHead(old, req.Expected); err != nil {
		return Result{}, err
	}
	entries, err := s.snapshot(old.Commit)
	if err != nil {
		return Result{}, err
	}
	m := mapTree(entries)
	changed := len(req.Puts) > 0
	for _, path := range req.Deletes {
		if _, ok := m[path]; ok {
			changed = true
		}
		delete(m, path)
	}
	if !changed {
		return Result{}, fail("invalid", "transaction changes nothing")
	}
	for path, b := range req.Puts {
		m[path] = Entry{path, Digest("blob", b), int64(len(b))}
	}
	entries = listTree(m)
	if err = validateTree(entries); err != nil {
		return Result{}, err
	}
	// Deterministic object-write order also makes fault injection reproducible.
	paths := make([]string, 0, len(req.Puts))
	for p := range req.Puts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if _, err = s.putObject("blob", req.Puts[p]); err != nil {
			return Result{}, err
		}
	}
	return s.publish("commit", req.Branch, actor, req.RequestID, fp, old, entries, []string{old.Commit})
}
func (s *Store) Fork(actor string, req ForkRequest) (Result, error) {
	if err := validMutation(actor, req.RequestID); err != nil {
		return Result{}, err
	}
	if !ValidBranch(req.Branch) || !ValidBranch(req.Source) {
		return Result{}, fail("invalid", "invalid branch")
	}
	fp, err := fingerprint("fork", actor, req)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.check(); err != nil {
		return Result{}, err
	}
	if r, ok, err := s.retry(actor, req.RequestID, fp); ok {
		return r, err
	}
	old, err := s.head(req.Source)
	if err != nil {
		return Result{}, err
	}
	if old != req.Expected {
		return Result{}, fail("conflict", "stale source head")
	}
	if _, ok := s.heads[req.Branch]; ok {
		return Result{}, fail("conflict", "branch exists")
	}
	if len(s.heads) >= MaxBranches {
		return Result{}, fail("limit", "branch limit")
	}
	next := Head{old.Commit, 1}
	if err = s.appendRecord("fork", req.Branch, actor, req.RequestID, fp, Head{}, next); err != nil {
		return Result{}, err
	}
	return Result{next, false}, nil
}
func (s *Store) Restore(actor string, req RestoreRequest) (Result, error) {
	if err := validMutation(actor, req.RequestID); err != nil {
		return Result{}, err
	}
	if !ValidBranch(req.Branch) || !ValidDigest(req.Commit) {
		return Result{}, fail("invalid", "invalid restore")
	}
	fp, err := fingerprint("restore", actor, req)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.check(); err != nil {
		return Result{}, err
	}
	if r, ok, err := s.retry(actor, req.RequestID, fp); ok {
		return r, err
	}
	old, err := s.head(req.Branch)
	if err != nil {
		return Result{}, err
	}
	if err = compareHead(old, req.Expected); err != nil {
		return Result{}, err
	}
	anc, err := s.ancestors(old.Commit)
	if err != nil {
		return Result{}, err
	}
	if _, ok := anc[req.Commit]; !ok {
		return Result{}, fail("conflict", "restore target is not an ancestor")
	}
	entries, err := s.snapshot(req.Commit)
	if err != nil {
		return Result{}, err
	}
	return s.publish("restore", req.Branch, actor, req.RequestID, fp, old, entries, []string{old.Commit})
}
