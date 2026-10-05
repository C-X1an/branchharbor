package store

import (
	"branchharbor/internal/strictjson"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func ensureDir(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err = os.Mkdir(path, 0700); err != nil {
			return err
		}
		return syncDir(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return fail("invalid", "unsafe storage directory")
	}
	return nil
}
func (s *Store) step(stage string) error {
	if s.hook != nil {
		return s.hook(stage)
	}
	return nil
}
func (s *Store) writeAtomic(path string, b []byte, kind string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { f.Close(); os.Remove(name) }()
	if err = s.step(kind + "-write"); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = s.step(kind + "-sync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.step(kind + "-rename"); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if err = s.step(kind + "-dir-sync"); err != nil {
		return err
	}
	return syncDir(dir)
}
func (s *Store) objectPath(kind, id string) (string, error) {
	if (kind != "blob" && kind != "tree" && kind != "commit") || !ValidDigest(id) {
		return "", fail("invalid", "invalid object identity")
	}
	return filepath.Join(s.dir, "objects", kind, id), nil
}
func (s *Store) readObject(kind, id string) ([]byte, error) {
	path, err := s.objectPath(kind, id)
	if err != nil {
		return nil, err
	}
	f, err := openPrivate(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	max := int64(MaxFileBytes)
	if kind != "blob" {
		max = MaxFrameBytes
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max || Digest(kind, b) != id {
		return nil, fail("unavailable", "object integrity failure")
	}
	return b, nil
}
func (s *Store) putObject(kind string, b []byte) (string, error) {
	id := Digest(kind, b)
	path, _ := s.objectPath(kind, id)
	if _, err := os.Lstat(path); err == nil {
		existing, err := s.readObject(kind, id)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, b) {
			return "", fail("unavailable", "object collision")
		}
		// An orphan may come from a prior attempt whose rename succeeded but
		// directory sync failed. Re-establish durability before reusing it.
		f, err := openPrivate(path, os.O_RDONLY)
		if err != nil {
			return "", err
		}
		if err = s.step("object-reuse-sync"); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		if err = s.step("object-reuse-dir-sync"); err != nil {
			return "", err
		}
		if err = syncDir(filepath.Dir(path)); err != nil {
			return "", err
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if s.diskBytes+int64(len(b)) > MaxStoreBytes {
		return "", fail("limit", "physical storage limit")
	}
	if err := s.writeAtomic(path, b, "object"); err != nil {
		// A rename may have succeeded before directory sync failed. Recount so failed
		// transactions cannot bypass the physical quota with orphan objects.
		if n, e := s.countBytes(); e == nil {
			s.diskBytes = n
		}
		return "", err
	}
	s.diskBytes += int64(len(b))
	return id, nil
}
func (s *Store) tree(id string) ([]Entry, error) {
	b, err := s.readObject("tree", id)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err = strictjson.Decode(b, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, fail("unavailable", "null tree")
	}
	canonical, err := CanonicalTree(entries)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, b) {
		return nil, fail("unavailable", "noncanonical tree")
	}
	return entries, nil
}
func (s *Store) commit(id string) (Commit, error) {
	var c Commit
	b, err := s.readObject("commit", id)
	if err != nil {
		return c, err
	}
	if err = strictjson.Decode(b, &c); err != nil {
		return c, err
	}
	if c.Schema != 1 || !ValidDigest(c.Tree) || c.Parents == nil || len(c.Parents) > 2 || !ValidID(c.Actor) || !ValidID(c.RequestID) {
		return c, fail("unavailable", "invalid commit")
	}
	for i, p := range c.Parents {
		if !ValidDigest(p) || p == id || i > 0 && p == c.Parents[0] {
			return c, fail("unavailable", "invalid commit parent")
		}
	}
	canon, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if !bytes.Equal(b, canon) {
		return c, fail("unavailable", "noncanonical commit")
	}
	return c, nil
}
func (s *Store) snapshot(id string) ([]Entry, error) {
	c, err := s.commit(id)
	if err != nil {
		return nil, err
	}
	return s.tree(c.Tree)
}
func (s *Store) countBytes() (int64, error) {
	var total int64
	for _, path := range []string{filepath.Join(s.dir, "objects"), filepath.Join(s.dir, "journal")} {
		err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fail("invalid", "symlink in store")
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			if d.IsDir() {
				if st.Mode().Perm()&0077 != 0 {
					return fail("invalid", "unsafe directory permission")
				}
				return nil
			}
			if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
				return fail("invalid", "unsafe object file")
			}
			total += st.Size()
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
func (s *Store) markReachable() (map[string]bool, error) {
	marked := map[string]bool{}
	seen := map[string]bool{}
	todo := []string{}
	for _, h := range s.heads {
		todo = append(todo, h.Commit)
	}
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(seen) > MaxAncestors {
			return nil, fail("limit", "ancestry limit")
		}
		c, err := s.commit(id)
		if err != nil {
			return nil, err
		}
		marked["commit/"+id] = true
		entries, err := s.tree(c.Tree)
		if err != nil {
			return nil, err
		}
		marked["tree/"+c.Tree] = true
		for _, e := range entries {
			key := "blob/" + e.Digest
			if !marked[key] {
				b, err := s.readObject("blob", e.Digest)
				if err != nil {
					return nil, err
				}
				if int64(len(b)) != e.Size {
					return nil, fail("unavailable", "blob size mismatch")
				}
				marked[key] = true
			} else {
				path, _ := s.objectPath("blob", e.Digest)
				st, err := os.Lstat(path)
				if err != nil || st.Size() != e.Size {
					return nil, fail("unavailable", "blob size mismatch")
				}
			}
		}
		todo = append(todo, c.Parents...)
	}
	return marked, nil
}
func (s *Store) GC(apply bool) (GCResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := GCResult{Applied: apply}
	if err := s.check(); err != nil {
		return r, err
	}
	marked, err := s.markReachable()
	if err != nil {
		return r, err
	}
	for _, kind := range []string{"blob", "tree", "commit"} {
		dir := filepath.Join(s.dir, "objects", kind)
		files, err := os.ReadDir(dir)
		if err != nil {
			return r, err
		}
		for _, f := range files {
			if f.IsDir() || f.Type()&os.ModeSymlink != 0 {
				return r, fail("invalid", "unexpected object entry")
			}
			if marked[kind+"/"+f.Name()] {
				continue
			}
			if !ValidDigest(f.Name()) && !strings.HasPrefix(f.Name(), ".tmp-") {
				return r, fail("invalid", "unknown object filename")
			}
			st, err := f.Info()
			if err != nil {
				return r, err
			}
			r.Candidates++
			r.Bytes += st.Size()
			if apply {
				if err = os.Remove(filepath.Join(dir, f.Name())); err != nil {
					return r, err
				}
			}
		}
		if apply {
			if err = syncDir(dir); err != nil {
				return r, err
			}
		}
	}
	if apply {
		n, err := s.countBytes()
		if err != nil {
			return r, err
		}
		s.diskBytes = n
	}
	return r, nil
}
