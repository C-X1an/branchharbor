package store

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

var mergeFuzzPaths = []string{"a", "b", "docs", "docs/a", "docs/b", "empty", "x/y"}

func mergeFuzzTree(input []byte, offset int) ([]Entry, map[string]byte) {
	states := map[string]byte{}
	for i, path := range mergeFuzzPaths {
		if offset+i < len(input) {
			states[path] = input[offset+i] % 4
		}
	}
	if states["docs"] != 0 {
		states["docs/a"], states["docs/b"] = 0, 0
	}
	entries := []Entry{}
	for _, path := range mergeFuzzPaths {
		if state := states[path]; state != 0 {
			entries = append(entries, mergeFuzzEntry(path, state))
		}
	}
	return entries, states
}

func mergeFuzzEntry(path string, state byte) Entry {
	// State 0 is absence, 1 is an empty file, 2/3 are distinct content.
	data := []byte{}
	if state > 1 {
		data = []byte{state}
	}
	return Entry{path, Digest("blob", data), int64(len(data))}
}

func FuzzMergeTrees(f *testing.F) {
	for base := byte(0); base < 4; base++ {
		for ours := byte(0); ours < 4; ours++ {
			for theirs := byte(0); theirs < 4; theirs++ {
				seed := make([]byte, 3*len(mergeFuzzPaths))
				seed[0], seed[len(mergeFuzzPaths)], seed[2*len(mergeFuzzPaths)] = base, ours, theirs
				f.Add(seed)
			}
		}
	}
	structural := make([]byte, 3*len(mergeFuzzPaths))
	structural[len(mergeFuzzPaths)+2] = 2   // ours adds docs as a file
	structural[2*len(mergeFuzzPaths)+3] = 3 // theirs adds docs/a
	f.Add(structural)
	f.Fuzz(func(t *testing.T, input []byte) {
		base, b := mergeFuzzTree(input, 0)
		ours, o := mergeFuzzTree(input, len(mergeFuzzPaths))
		theirs, th := mergeFuzzTree(input, 2*len(mergeFuzzPaths))
		originalBase := append([]Entry{}, base...)
		originalOurs := append([]Entry{}, ours...)
		originalTheirs := append([]Entry{}, theirs...)
		for _, tree := range [][]Entry{base, ours, theirs} {
			if err := validateTree(tree); err != nil {
				t.Fatalf("generator made invalid tree: %v", err)
			}
		}
		// Reference decisions use absent/content states rather than the
		// implementation's entry comparison and maps.
		wanted := []Entry{}
		conflicting := map[string]bool{}
		for _, path := range mergeFuzzPaths {
			state := o[path]
			if o[path] != th[path] {
				if o[path] == b[path] {
					state = th[path]
				} else if th[path] != b[path] {
					conflicting[path] = true
					continue
				}
			}
			if state != 0 {
				wanted = append(wanted, mergeFuzzEntry(path, state))
			}
		}
		for i, entry := range wanted {
			for _, other := range wanted[i+1:] {
				if strings.HasPrefix(other.Path, entry.Path+"/") {
					conflicting[entry.Path], conflicting[other.Path] = true, true
				}
			}
		}
		conflicts := []string{}
		for path := range conflicting {
			conflicts = append(conflicts, path)
		}
		sort.Strings(conflicts)
		got, paths, err := mergeTrees(base, ours, theirs)
		if err != nil || !reflect.DeepEqual(got, wanted) || !reflect.DeepEqual(paths, conflicts) {
			t.Fatalf("merge differs from reference: entries=%v want=%v conflicts=%v want=%v err=%v", got, wanted, paths, conflicts, err)
		}
		swapped, swappedPaths, swappedErr := mergeTrees(base, theirs, ours)
		if swappedErr != nil || !reflect.DeepEqual(swapped, got) || !reflect.DeepEqual(swappedPaths, paths) {
			t.Fatal("merge is not symmetric")
		}
		for _, pair := range [][2][]Entry{{ours, ours}, {base, theirs}} {
			identity, identityPaths, identityErr := mergeTrees(base, pair[0], pair[1])
			if identityErr != nil || len(identityPaths) != 0 || !reflect.DeepEqual(identity, pair[1]) {
				t.Fatal("merge identity failed")
			}
		}
		if !reflect.DeepEqual(base, originalBase) || !reflect.DeepEqual(ours, originalOurs) || !reflect.DeepEqual(theirs, originalTheirs) {
			t.Fatal("merge mutated its inputs")
		}
	})
}
