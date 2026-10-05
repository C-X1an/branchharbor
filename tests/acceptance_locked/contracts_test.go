// These public-interface tests are implementation work derived from the locked
// acceptance document. Their author is not an independent verifier.
package acceptance_test

import (
	"branchharbor/internal/store"
	"path/filepath"
	"testing"
)

func TestAC002_AtomicHistory(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, err := s.Head("main")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Commit("client", store.CommitRequest{Branch: "main", Expected: old, RequestID: "first", Puts: map[string][]byte{"a": []byte("A"), "empty": {}}})
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err := s.Snapshot("main")
	if err != nil || len(entries) != 2 {
		t.Fatal("atomic contents missing")
	}
	if _, err = s.ReadAt(old.Commit, "a"); store.Code(err) != "not_found" {
		t.Fatal("old snapshot mutated")
	}
	head, b, err := s.Read("main", "empty")
	if err != nil || head != result.Head || len(b) != 0 {
		t.Fatal("empty file confused with missing file")
	}
}
func TestAC004_RetryAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Head("main")
	if err != nil {
		t.Fatal(err)
	}
	req := store.CommitRequest{Branch: "main", Expected: h, RequestID: "once", Puts: map[string][]byte{"a": []byte("A")}}
	first, err := s.Commit("client", req)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err := s.Commit("client", req)
	if err != nil || again.Head != first.Head || !again.Replayed {
		t.Fatal("durable idempotency failed")
	}
	req.Puts = map[string][]byte{"a": []byte("B")}
	if _, err = s.Commit("client", req); store.Code(err) != "idempotency_conflict" {
		t.Fatal("changed retry accepted")
	}
}
