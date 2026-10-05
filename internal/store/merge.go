package store

import "sort"

func (s *Store) ancestors(id string) (map[string]Commit, error) {
	out := map[string]Commit{}
	todo := []string{id}
	for len(todo) > 0 {
		v := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if _, ok := out[v]; ok {
			continue
		}
		if len(out) >= MaxAncestors {
			return nil, fail("limit", "ancestry limit")
		}
		c, err := s.commit(v)
		if err != nil {
			return nil, err
		}
		out[v] = c
		todo = append(todo, c.Parents...)
	}
	return out, nil
}
func (s *Store) mergeBase(a, b string) (string, error) {
	aa, err := s.ancestors(a)
	if err != nil {
		return "", err
	}
	bb, err := s.ancestors(b)
	if err != nil {
		return "", err
	}
	// Common ancestors form an ancestry-closed set. Each immediate parent of a
	// common node is therefore non-minimal. Remaining nodes are best bases.
	common := map[string]Commit{}
	nonminimal := map[string]bool{}
	for id, c := range aa {
		if _, ok := bb[id]; ok {
			common[id] = c
		}
	}
	for _, c := range common {
		for _, p := range c.Parents {
			nonminimal[p] = true
		}
	}
	bases := []string{}
	for id := range common {
		if !nonminimal[id] {
			bases = append(bases, id)
		}
	}
	if len(bases) == 0 {
		return "", fail("conflict", "no common ancestor")
	}
	if len(bases) != 1 {
		return "", fail("ambiguous_base", "multiple merge bases require explicit reconciliation")
	}
	return bases[0], nil
}
func sameEntry(a Entry, ap bool, b Entry, bp bool) bool { return ap == bp && (!ap || a == b) }
func mergeTrees(base, ours, theirs []Entry) ([]Entry, []string, error) {
	bm, om, tm := mapTree(base), mapTree(ours), mapTree(theirs)
	paths := map[string]bool{}
	for _, m := range []map[string]Entry{bm, om, tm} {
		for p := range m {
			paths[p] = true
		}
	}
	out := map[string]Entry{}
	conflicts := []string{}
	for p := range paths {
		b, bp := bm[p]
		o, op := om[p]
		t, tp := tm[p]
		switch {
		case sameEntry(o, op, t, tp):
			if op {
				out[p] = o
			}
		case sameEntry(o, op, b, bp):
			if tp {
				out[p] = t
			}
		case sameEntry(t, tp, b, bp):
			if op {
				out[p] = o
			}
		default:
			conflicts = append(conflicts, p)
		}
	}
	entries := listTree(out)
	if err := validateTree(entries); err != nil {
		if Code(err) == "conflict" {
			for _, e := range entries {
				for i := 0; i < len(e.Path); i++ {
					if e.Path[i] == '/' {
						if _, ok := out[e.Path[:i]]; ok {
							conflicts = append(conflicts, e.Path, e.Path[:i])
						}
					}
				}
			}
		} else {
			return nil, nil, err
		}
	}
	sort.Strings(conflicts)
	unique := conflicts[:0]
	for _, p := range conflicts {
		if len(unique) == 0 || unique[len(unique)-1] != p {
			unique = append(unique, p)
		}
	}
	return entries, unique, nil
}
func (s *Store) preview(target, source string) (Preview, []Entry, error) {
	p := Preview{Conflicts: []string{}}
	if target == source {
		return p, nil, fail("invalid", "source equals target")
	}
	t, err := s.head(target)
	if err != nil {
		return p, nil, err
	}
	u, err := s.head(source)
	if err != nil {
		return p, nil, err
	}
	p.Target = t
	p.Source = u
	p.Base, err = s.mergeBase(t.Commit, u.Commit)
	if err != nil {
		return p, nil, err
	}
	base, err := s.snapshot(p.Base)
	if err != nil {
		return p, nil, err
	}
	ours, err := s.snapshot(t.Commit)
	if err != nil {
		return p, nil, err
	}
	theirs, err := s.snapshot(u.Commit)
	if err != nil {
		return p, nil, err
	}
	entries, conflicts, err := mergeTrees(base, ours, theirs)
	p.Conflicts = conflicts
	return p, entries, err
}
func (s *Store) Preview(target, source string) (Preview, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(); err != nil {
		return Preview{}, err
	}
	p, _, err := s.preview(target, source)
	return p, err
}
func (s *Store) Merge(actor string, req MergeRequest) (Result, error) {
	if err := validMutation(actor, req.RequestID); err != nil {
		return Result{}, err
	}
	if !ValidBranch(req.Target) || !ValidBranch(req.Source) {
		return Result{}, fail("invalid", "invalid branch")
	}
	fp, err := fingerprint("merge", actor, req)
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
	p, entries, err := s.preview(req.Target, req.Source)
	if err != nil {
		return Result{}, err
	}
	if err = compareHead(p.Target, req.ExpectedTarget); err != nil {
		return Result{}, err
	}
	if p.Source != req.ExpectedSource {
		return Result{}, fail("conflict", "stale source head")
	}
	if len(p.Conflicts) > 0 {
		return Result{}, fail("conflict", "merge has conflicting paths")
	}
	if p.Target.Commit == p.Source.Commit {
		return Result{}, fail("invalid", "branches already share a commit")
	}
	return s.publish("merge", req.Target, actor, req.RequestID, fp, p.Target, entries, []string{p.Target.Commit, p.Source.Commit})
}
