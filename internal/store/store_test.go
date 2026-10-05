package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func headTest(t *testing.T, s *Store, b string) Head {
	t.Helper()
	h, err := s.Head(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func putTest(t *testing.T, s *Store, b, id, path, value string) Head {
	t.Helper()
	r, err := s.Commit("owner", CommitRequest{b, headTest(t, s, b), id, map[string][]byte{path: []byte(value)}, nil})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head
}
func forkTest(t *testing.T, s *Store, b, source, id string) Head {
	t.Helper()
	r, err := s.Fork("owner", ForkRequest{b, source, headTest(t, s, source), id})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head
}
func codeTest(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || Code(err) != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}
func TestObjects(t *testing.T) {
	// Independent constants generated with Python hashlib, not the implementation.
	vectors := map[string]string{"": "5547c44c5d00eb12db0f9a53f81c60ff1d5fcf80819abcca5fa181e1979cbb2e", "abc": "c9b1336e345f6286186f43399ab68db912510930ca0c8d61d04f454313f28159"}
	for b, want := range vectors {
		if got := Digest("blob", []byte(b)); got != want {
			t.Fatalf("hash mismatch: %s", got)
		}
	}
	if Digest("blob", []byte("abc")) == Digest("tree", []byte("abc")) {
		t.Fatal("missing domain separation")
	}
	s := openTest(t)
	h := putTest(t, s, "main", "one", "a", "abc")
	c, err := s.commit(h.Commit)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.tree(c.Tree)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.objectPath("blob", entries[0].Digest)
	if err = os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Read("main", "a")
	codeTest(t, err, "unavailable")
}
func TestCanonical(t *testing.T) {
	a := Entry{"z", Digest("blob", []byte("z")), 1}
	b := Entry{"a", Digest("blob", []byte{}), 0}
	x, err := CanonicalTree([]Entry{a, b})
	if err != nil {
		t.Fatal(err)
	}
	y, _ := CanonicalTree([]Entry{b, a})
	if !bytes.Equal(x, y) {
		t.Fatal("order affects tree")
	}
	e, _ := CanonicalTree(nil)
	if string(e) != "[]" {
		t.Fatal("empty tree must be []")
	}
	_, err = CanonicalTree([]Entry{a, a})
	codeTest(t, err, "invalid")
}
func TestAtomicCommit(t *testing.T) {
	s := openTest(t)
	old := putTest(t, s, "main", "seed", "old", "old-content")
	r, err := s.Commit("owner", CommitRequest{"main", old, "batch", map[string][]byte{"a": []byte("alpha"), "b": []byte("beta")}, []string{"old"}})
	if err != nil {
		t.Fatal(err)
	}
	h, entries, err := s.Snapshot("main")
	if err != nil || h != r.Head || len(entries) != 2 || entries[0].Path != "a" || entries[1].Path != "b" {
		t.Fatalf("incomplete atomic snapshot: %v %v", entries, err)
	}
	b, err := s.ReadAt(old.Commit, "old")
	if err != nil || string(b) != "old-content" {
		t.Fatal("historical snapshot changed", err)
	}
	_, _, err = s.Read("main", "old")
	codeTest(t, err, "not_found")
	_, err = s.Commit("owner", CommitRequest{"main", r.Head, "invalid-batch", map[string][]byte{"safe": []byte("x"), "../bad": []byte("bad")}, nil})
	codeTest(t, err, "invalid")
	if headTest(t, s, "main") != r.Head {
		t.Fatal("invalid batch changed head")
	}
}
func TestConcurrentCAS(t *testing.T) {
	s := openTest(t)
	old := headTest(t, s, "main")
	const n = 24
	start := make(chan struct{})
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Commit("owner", CommitRequest{"main", old, fmt.Sprintf("race-%d", i), map[string][]byte{fmt.Sprintf("file%d", i): []byte("x")}, nil})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			codeTest(t, err, "conflict")
		}
	}
	if success != 1 {
		t.Fatalf("CAS accepted %d writers", success)
	}
	h, entries, err := s.Snapshot("main")
	if err != nil || h.Version != old.Version+1 || len(entries) != 1 {
		t.Fatal("CAS state incorrect")
	}
}
func TestIdempotency(t *testing.T) {
	s := openTest(t)
	req := CommitRequest{"main", headTest(t, s, "main"), "once", map[string][]byte{"a": []byte("1")}, nil}
	r, err := s.Commit("owner", req)
	if err != nil {
		t.Fatal(err)
	}
	putTest(t, s, "main", "later", "b", "2")
	for i := 0; i < 2; i++ {
		again, err := s.Commit("owner", req)
		if err != nil || !again.Replayed || again.Head != r.Head {
			t.Fatal("retry lost original result", again, err)
		}
		dir := s.dir
		s.Close()
		s, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
	}
	changed := req
	changed.Puts = map[string][]byte{"a": []byte("different")}
	_, err = s.Commit("owner", changed)
	codeTest(t, err, "idempotency_conflict")
	changed.RequestID = "new"
	_, err = s.Commit("owner", changed)
	codeTest(t, err, "conflict")
}
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, "objects"), func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestForkSharing(t *testing.T) {
	s := openTest(t)
	a := putTest(t, s, "main", "seed", "a", strings.Repeat("x", 8192))
	n := objectCount(t, s.dir)
	b := forkTest(t, s, "worker", "main", "fork")
	if a.Commit != b.Commit || b.Version != 1 || n != objectCount(t, s.dir) {
		t.Fatal("fork copied or changed objects")
	}
	putTest(t, s, "worker", "worker-write", "a", "new")
	_, v, err := s.Read("main", "a")
	if err != nil || len(v) != 8192 {
		t.Fatal("fork mutated main")
	}
}
func TestMergeDisjoint(t *testing.T) {
	s := openTest(t)
	forkTest(t, s, "worker", "main", "fork")
	a := putTest(t, s, "main", "main-a", "a", "A")
	b := putTest(t, s, "worker", "worker-b", "b", "B")
	p, err := s.Preview("main", "worker")
	if err != nil || len(p.Conflicts) != 0 {
		t.Fatal(p, err)
	}
	r, err := s.Merge("owner", MergeRequest{"main", "worker", a, b, "merge"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.commit(r.Head.Commit)
	if err != nil || !reflect.DeepEqual(c.Parents, []string{a.Commit, b.Commit}) {
		t.Fatal("merge ancestry", err)
	}
	_, files, err := s.Snapshot("main")
	if err != nil || len(files) != 2 {
		t.Fatal("missing merged files")
	}
}
func TestMergeConflict(t *testing.T) {
	for _, mode := range []string{"modify-modify", "modify-delete", "structural"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t)
			putTest(t, s, "main", "seed", "a", "base")
			forkTest(t, s, "worker", "main", "fork")
			var a, b Head
			switch mode {
			case "modify-modify":
				a = putTest(t, s, "main", "m", "a", "ours")
				b = putTest(t, s, "worker", "w", "a", "theirs")
			case "modify-delete":
				a = putTest(t, s, "main", "m", "a", "ours")
				r, e := s.Commit("owner", CommitRequest{"worker", headTest(t, s, "worker"), "w", nil, []string{"a"}})
				if e != nil {
					t.Fatal(e)
				}
				b = r.Head
			case "structural":
				a = putTest(t, s, "main", "m", "docs", "file")
				b = putTest(t, s, "worker", "w", "docs/x", "child")
			}
			p, err := s.Preview("main", "worker")
			if err != nil || len(p.Conflicts) == 0 {
				t.Fatal("conflict not previewed", p, err)
			}
			_, err = s.Merge("owner", MergeRequest{"main", "worker", a, b, "merge"})
			codeTest(t, err, "conflict")
			if headTest(t, s, "main") != a {
				t.Fatal("conflicting merge changed head")
			}
		})
	}
}
func TestAmbiguousBase(t *testing.T) {
	s := openTest(t)
	forkTest(t, s, "b", "main", "fork-b")
	a := putTest(t, s, "main", "a1", "a", "A")
	b := putTest(t, s, "b", "b1", "b", "B")
	forkTest(t, s, "a-old", "main", "save-a")
	forkTest(t, s, "b-old", "b", "save-b")
	if _, err := s.Merge("owner", MergeRequest{"main", "b-old", a, headTest(t, s, "b-old"), "merge-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Merge("owner", MergeRequest{"b", "a-old", b, headTest(t, s, "a-old"), "merge-b"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Preview("main", "b")
	codeTest(t, err, "ambiguous_base")
}
func TestRestore(t *testing.T) {
	s := openTest(t)
	a := putTest(t, s, "main", "a", "a", "old")
	b := putTest(t, s, "main", "b", "a", "new")
	r, err := s.Restore("owner", RestoreRequest{"main", a.Commit, b, "restore"})
	if err != nil || r.Head.Version != b.Version+1 || r.Head.Commit == a.Commit {
		t.Fatal("restore reset history", r, err)
	}
	c, err := s.commit(r.Head.Commit)
	if err != nil || c.Parents[0] != b.Commit {
		t.Fatal("restore ancestry")
	}
	_, v, err := s.Read("main", "a")
	if err != nil || string(v) != "old" {
		t.Fatal("restore contents")
	}
}
func TestGC(t *testing.T) {
	s := openTest(t)
	old := putTest(t, s, "main", "old", "a", "old")
	putTest(t, s, "main", "new", "a", "new")
	orphan, err := s.putObject("blob", []byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.GC(false)
	if err != nil || r.Candidates != 1 || r.Bytes != 6 {
		t.Fatal(r, err)
	}
	path, _ := s.objectPath("blob", orphan)
	if _, err = os.Stat(path); err != nil {
		t.Fatal("dry run removed orphan")
	}
	r, err = s.GC(true)
	if err != nil || r.Candidates != 1 {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan remains")
	}
	b, err := s.ReadAt(old.Commit, "a")
	if err != nil || string(b) != "old" {
		t.Fatal("GC removed historical object")
	}
}
func TestPersistenceOrder(t *testing.T) {
	s := openTest(t)
	stages := []string{}
	s.hook = func(v string) error { stages = append(stages, v); return nil }
	putTest(t, s, "main", "ordered", "a", "unique")
	want := []string{}
	for i := 0; i < 3; i++ {
		want = append(want, "object-write", "object-sync", "object-rename", "object-dir-sync")
	}
	want = append(want, "journal-write", "after-journal-write", "journal-sync", "after-journal-sync")
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("persistence order\ngot %v\nwant%v", stages, want)
	}
}
func TestPoisonedStore(t *testing.T) {
	for _, stage := range []string{"journal-write", "journal-sync", "after-journal-sync"} {
		t.Run(stage, func(t *testing.T) {
			s := openTest(t)
			old := headTest(t, s, "main")
			s.hook = func(v string) error {
				if v == stage {
					return io.ErrClosedPipe
				}
				return nil
			}
			_, err := s.Commit("owner", CommitRequest{"main", old, "uncertain", map[string][]byte{"a": []byte("x")}, nil})
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal("did not propagate injected failure", err)
			}
			if s.Ready() {
				t.Fatal("store not poisoned")
			}
			s.hook = nil
			_, err = s.Commit("owner", CommitRequest{"main", old, "next", map[string][]byte{"b": []byte("y")}, nil})
			codeTest(t, err, "unavailable")
			dir := s.dir
			s.Close()
			next, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			next.Close()
		})
	}
}
func TestObjectFailureLeavesNoPublishedHead(t *testing.T) {
	for _, stage := range []string{"object-write", "object-sync", "object-rename", "object-dir-sync"} {
		t.Run(stage, func(t *testing.T) {
			s := openTest(t)
			old := headTest(t, s, "main")
			s.hook = func(v string) error {
				if v == stage {
					return io.ErrClosedPipe
				}
				return nil
			}
			_, err := s.Commit("owner", CommitRequest{"main", old, "failed", map[string][]byte{"a": []byte("x")}, nil})
			if err == nil {
				t.Fatal("injection ignored")
			}
			s.hook = nil
			if headTest(t, s, "main") != old {
				t.Fatal("head published before object persistence")
			}
		})
	}
}
func TestTornTail(t *testing.T) {
	s := openTest(t)
	baseHead := headTest(t, s, "main")
	base, err := os.ReadFile(filepath.Join(s.dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	putTest(t, s, "main", "tail", "a", "x")
	all, err := os.ReadFile(filepath.Join(s.dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	tail := all[len(base):]
	dir := s.dir
	s.Close()
	for i := 1; i < len(tail); i++ {
		if err = os.WriteFile(filepath.Join(dir, "journal"), append(bytes.Clone(base), tail[:i]...), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("prefix %d: %v", i, err)
		}
		h := headTest(t, r, "main")
		r.Close()
		if h != baseHead {
			t.Fatalf("prefix %d applied incomplete record", i)
		}
		st, err := os.Stat(filepath.Join(dir, "journal"))
		if err != nil || st.Size() != int64(len(base)) {
			t.Fatalf("prefix %d not truncated", i)
		}
	}
	t.Logf("validated all %d nonempty incomplete frame prefixes", len(tail)-1)
}
func TestJournalCorruption(t *testing.T) {
	for _, mode := range []string{"checksum", "sequence", "chain", "old-head", "magic", "length", "length-plus-one", "length-plus-512", "length-and-checksum", "length-and-json"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t)
			base, err := os.ReadFile(filepath.Join(s.dir, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			old := headTest(t, s, "main")
			acknowledged := putTest(t, s, "main", "change", "a", "value")
			all, err := os.ReadFile(filepath.Join(s.dir, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			tail := bytes.Clone(all[len(base):])
			dir := s.dir
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "checksum":
				tail[len(tail)-1] ^= 1
			case "magic":
				tail[0] = 'X'
			case "length":
				binary.BigEndian.PutUint32(tail[4:8], MaxFrameBytes+1)
			case "length-plus-one", "length-and-checksum", "length-and-json":
				binary.BigEndian.PutUint32(tail[4:8], binary.BigEndian.Uint32(tail[4:8])+1)
				if mode == "length-and-checksum" {
					tail[len(tail)-1] ^= 1
				}
				if mode == "length-and-json" {
					tail[8] = '!'
				}
			case "length-plus-512":
				binary.BigEndian.PutUint32(tail[4:8], binary.BigEndian.Uint32(tail[4:8])+512)
			default:
				var r record
				length := binary.BigEndian.Uint32(tail[4:8])
				if err = json.Unmarshal(tail[8:8+length], &r); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "sequence":
					r.Seq += 2
				case "chain":
					r.PrevHash = strings.Repeat("0", 64)
				case "old-head":
					r.Old.Version += 1
				}
				p, marshalErr := json.Marshal(r)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				tail = encodeFrame(p)
			}
			corrupt := append(bytes.Clone(base), tail...)
			if err = os.WriteFile(filepath.Join(dir, "journal"), corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			next, err := Open(dir)
			if err == nil {
				if closeErr := next.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				t.Fatal("complete corruption accepted")
			}
			after, readErr := os.ReadFile(filepath.Join(dir, "journal"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, corrupt) {
				t.Fatal("corruption silently rewritten")
			}
			// Recover the preserved original fixture, then verify both the
			// acknowledged head and its original request identity survive.
			if err = os.WriteFile(filepath.Join(dir, "journal"), all, 0600); err != nil {
				t.Fatal(err)
			}
			restored, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			result, retryErr := restored.Commit("owner", CommitRequest{"main", old, "change", map[string][]byte{"a": []byte("value")}, nil})
			restoredHead := headTest(t, restored, "main")
			if closeErr := restored.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if retryErr != nil || !result.Replayed || result.Head != acknowledged || restoredHead != acknowledged {
				t.Fatalf("verified original lost acknowledged head or retry: result=%+v error=%v", result, retryErr)
			}
		})
	}
}
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("BH_TEST_HELPER")
	if mode == "" {
		return
	}
	dir := os.Getenv("BH_TEST_DIR")
	s, err := Open(dir)
	if mode == "locked" {
		if Code(err) == "unavailable" {
			fmt.Println("LOCK_REJECTED")
			os.Exit(0)
		}
		os.Exit(4)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	stage := os.Getenv("BH_TEST_STAGE")
	if mode == "crash" {
		s.hook = func(v string) error {
			if v == stage {
				syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}
	}
	h, err := s.Head("main")
	if err != nil {
		os.Exit(5)
	}
	_, err = s.Commit("child", CommitRequest{"main", h, "child-change", map[string][]byte{"child": []byte("durable")}, nil})
	if err != nil {
		os.Exit(6)
	}
	fmt.Println("ACKNOWLEDGED")
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	os.Exit(7)
}
func helper(dir, mode, stage string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	c.Env = append(os.Environ(), "BH_TEST_HELPER="+mode, "BH_TEST_DIR="+dir, "BH_TEST_STAGE="+stage)
	return c
}
func TestSingleWriter(t *testing.T) {
	s := openTest(t)
	b, err := helper(s.dir, "locked", "").CombinedOutput()
	if err != nil || !bytes.Contains(b, []byte("LOCK_REJECTED")) {
		t.Fatalf("second process acquired store: %s %v", b, err)
	}
}
func TestCrashRecovery(t *testing.T) {
	for _, stage := range []string{"object-sync", "object-dir-sync", "journal-write", "after-journal-write", "after-journal-sync", "acknowledged"} {
		t.Run(stage, func(t *testing.T) {
			s := openTest(t)
			old := headTest(t, s, "main")
			dir := s.dir
			s.Close()
			b, err := helper(dir, "crash", stage).CombinedOutput()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("child was not killed: %s %v", b, err)
			}
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := headTest(t, s, "main")
			acknowledged := bytes.Contains(b, []byte("ACKNOWLEDGED"))
			if stage == "acknowledged" && !acknowledged {
				t.Fatal("child never acknowledged")
			}
			if acknowledged || stage == "after-journal-sync" {
				if h.Version != old.Version+1 {
					t.Fatal("durable acknowledged commit lost")
				}
			}
			if h != old {
				_, v, e := s.Read("main", "child")
				if e != nil || string(v) != "durable" {
					t.Fatal("partial recovered snapshot")
				}
				again, e := s.Commit("child", CommitRequest{"main", old, "child-change", map[string][]byte{"child": []byte("durable")}, nil})
				if e != nil || !again.Replayed || again.Head != h {
					t.Fatal("recovered idempotency failure", again, e)
				}
			}
		})
	}
}
func TestLimits(t *testing.T) {
	s := openTest(t)
	h := headTest(t, s, "main")
	_, err := s.Commit("o", CommitRequest{"main", h, "huge", map[string][]byte{"a": make([]byte, MaxFileBytes+1)}, nil})
	codeTest(t, err, "limit")
	puts := map[string][]byte{}
	for i := 0; i <= MaxChanges; i++ {
		puts[fmt.Sprintf("f%d", i)] = []byte("x")
	}
	_, err = s.Commit("o", CommitRequest{"main", h, "many", puts, nil})
	codeTest(t, err, "limit")
	for i := 1; i < MaxBranches; i++ {
		forkTest(t, s, fmt.Sprintf("b%d", i), "main", fmt.Sprintf("fork%d", i))
	}
	_, err = s.Fork("o", ForkRequest{"extra", "main", h, "over"})
	codeTest(t, err, "limit")
	entries := make([]Entry, MaxPaths+1)
	for i := range entries {
		entries[i] = Entry{fmt.Sprintf("p%04d", i), strings.Repeat("a", 64), 1}
	}
	codeTest(t, validateTree(entries), "limit")
	entries = entries[:17]
	for i := range entries {
		entries[i].Size = MaxFileBytes
	}
	codeTest(t, validateTree(entries), "limit")
	original := s.diskBytes
	s.diskBytes = MaxStoreBytes
	_, err = s.putObject("blob", []byte("quota"))
	codeTest(t, err, "limit")
	s.diskBytes = original
	s.journalBytes = MaxJournalBytes
	_, err = s.Commit("o", CommitRequest{"main", h, "journal-quota", map[string][]byte{"a": []byte("valid")}, nil})
	codeTest(t, err, "limit")
	if headTest(t, s, "main") != h {
		t.Fatal("quota failure changed head")
	}
}
func TestPaths(t *testing.T) {
	for _, p := range []string{"../outside", "/etc/passwd", "a//b", "a/../b", "a\\b", "a/%2e%2e/b", ".", "..", "a/", "", "a\x00b", "caf\u00e9"} {
		if ValidPath(p) {
			t.Fatalf("accepted unsafe path %q", p)
		}
	}
	for _, p := range []string{"a", "a/b.txt", ".hidden", "a/..suffix"} {
		if !ValidPath(p) {
			t.Fatalf("rejected valid path %q", p)
		}
	}
	s := openTest(t)
	putTest(t, s, "main", "file", "docs", "file")
	_, err := s.Commit("o", CommitRequest{"main", headTest(t, s, "main"), "collision", map[string][]byte{"docs/a": []byte("x")}, nil})
	codeTest(t, err, "conflict")
}
func TestBackupRestore(t *testing.T) {
	s := openTest(t)
	h := putTest(t, s, "main", "backup", "a", "snapshot")
	dir := s.dir
	s.Close()
	copyDir := filepath.Join(t.TempDir(), "restored")
	if err := copyStore(dir, copyDir); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if headTest(t, restored, "main") != h {
		t.Fatal("backup lost head")
	}
	_, b, err := restored.Read("main", "a")
	if err != nil || string(b) != "snapshot" {
		t.Fatal("backup lost data")
	}
}
func copyStore(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0600)
	})
}
func TestRandomReferenceModel(t *testing.T) {
	s := openTest(t)
	rng := rand.New(rand.NewSource(20261004))
	model := map[string]string{}
	for n := 0; n < 150; n++ {
		path := fmt.Sprintf("f%02d", rng.Intn(24))
		req := CommitRequest{Branch: "main", Expected: headTest(t, s, "main"), RequestID: fmt.Sprintf("random%d", n)}
		if _, ok := model[path]; ok && rng.Intn(3) == 0 {
			req.Deletes = []string{path}
			delete(model, path)
		} else {
			v := fmt.Sprintf("%d-%d", n, rng.Int63())
			req.Puts = map[string][]byte{path: []byte(v)}
			model[path] = v
		}
		if _, err := s.Commit("model", req); err != nil {
			t.Fatal(err)
		}
		_, entries, err := s.Snapshot("main")
		if err != nil || len(entries) != len(model) {
			t.Fatal("reference cardinality mismatch")
		}
		for path, want := range model {
			_, b, err := s.Read("main", path)
			if err != nil || string(b) != want {
				t.Fatalf("reference mismatch %s", path)
			}
		}
		if n%30 == 0 {
			dir := s.dir
			s.Close()
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
		}
	}
}
func TestUnsafeLayout(t *testing.T) {
	for _, which := range []string{"root-symlink", "object-symlink", "public-root", "unformatted", "bad-format"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "root-symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "public-root":
				os.Chmod(dir, 0755)
			case "unformatted":
				os.WriteFile(filepath.Join(dir, "unknown"), []byte("x"), 0600)
			case "bad-format":
				os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("unknown"), 0600)
			case "object-symlink":
				s, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				os.Symlink("/etc/passwd", filepath.Join(dir, "objects", "blob", strings.Repeat("a", 64)))
			}
			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatal("unsafe layout accepted")
			}
		})
	}
}
func TestCRCIndependentVector(t *testing.T) {
	if crc32.Checksum([]byte("123456789"), crcTable) != 0xe3069283 {
		t.Fatal("not CRC32C")
	}
}
func FuzzPath(f *testing.F) {
	for _, s := range []string{"a/b", "../escape", "/etc/passwd", "a\\b", "docs/%2e%2e", "", ".hidden"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if !ValidPath(p) {
			return
		}
		if filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\\%\x00") || len(p) > 240 {
			t.Fatalf("unsafe accepted path %q", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("unsafe segment %q", seg)
			}
		}
	})
}

func TestOrphanReuseResynchronizes(t *testing.T) {
	s := openTest(t)
	old := headTest(t, s, "main")
	req := CommitRequest{"main", old, "retry-orphan", map[string][]byte{"a": []byte("previously-unsynced")}, nil}
	s.hook = func(stage string) error {
		if stage == "object-dir-sync" {
			return io.ErrClosedPipe
		}
		return nil
	}
	if _, err := s.Commit("owner", req); err == nil {
		t.Fatal("first attempt should fail")
	}
	stages := []string{}
	s.hook = func(stage string) error { stages = append(stages, stage); return nil }
	if _, err := s.Commit("owner", req); err != nil {
		t.Fatal(err)
	}
	reuse, journal := -1, -1
	for i, stage := range stages {
		if stage == "object-reuse-dir-sync" {
			reuse = i
		}
		if stage == "journal-write" {
			journal = i
		}
	}
	if reuse < 0 || journal < 0 || reuse >= journal {
		t.Fatal("orphan reused before durability re-established", stages)
	}
}
