// Package store implements a bounded, single-owner versioned workspace store.
// Its API is trusted in-process; the HTTP layer is the authorization boundary.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const (
	MaxFileBytes    = 4 << 20
	MaxPaths        = 2048
	MaxLogicalBytes = 64 << 20
	MaxChanges      = 256
	MaxBranches     = 64
	MaxStoreBytes   = 256 << 20
	MaxJournalBytes = 32 << 20
	MaxFrameBytes   = 1 << 20
	MaxAncestors    = 100000
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string        { return e.Message }
func fail(code, message string) error { return &Error{code, message} }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}

type Head struct {
	Commit  string `json:"commit"`
	Version uint64 `json:"version"`
}
type Entry struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type Commit struct {
	Schema    int      `json:"schema"`
	Tree      string   `json:"tree"`
	Parents   []string `json:"parents"`
	Actor     string   `json:"actor"`
	RequestID string   `json:"request_id"`
}
type Result struct {
	Head     Head `json:"head"`
	Replayed bool `json:"replayed"`
}
type Preview struct {
	Base      string   `json:"base"`
	Target    Head     `json:"target"`
	Source    Head     `json:"source"`
	Conflicts []string `json:"conflicts"`
}
type GCResult struct {
	Candidates int   `json:"candidates"`
	Bytes      int64 `json:"bytes"`
	Applied    bool  `json:"applied"`
}

type CommitRequest struct {
	Branch    string            `json:"branch"`
	Expected  Head              `json:"expected"`
	RequestID string            `json:"request_id"`
	Puts      map[string][]byte `json:"puts"`
	Deletes   []string          `json:"deletes"`
}
type ForkRequest struct {
	Branch    string `json:"branch"`
	Source    string `json:"source"`
	Expected  Head   `json:"expected"`
	RequestID string `json:"request_id"`
}
type MergeRequest struct {
	Target         string `json:"target"`
	Source         string `json:"source"`
	ExpectedTarget Head   `json:"expected_target"`
	ExpectedSource Head   `json:"expected_source"`
	RequestID      string `json:"request_id"`
}
type RestoreRequest struct {
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`
	Expected  Head   `json:"expected"`
	RequestID string `json:"request_id"`
}
type operation struct {
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Request any    `json:"request"`
}

func ValidID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
func ValidBranch(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func ValidPath(s string) bool {
	if len(s) < 1 || len(s) > 240 {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range []byte(part) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
				return false
			}
		}
	}
	return true
}
func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func Digest(kind string, b []byte) string {
	h := sha256.New()
	h.Write([]byte("branchharbor/v1/" + kind + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func fingerprint(kind, actor string, req any) (string, error) {
	b, err := json.Marshal(operation{kind, actor, req})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func CanonicalTree(entries []Entry) ([]byte, error) {
	out := append([]Entry{}, entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if err := validateTree(out); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
func validateTree(entries []Entry) error {
	if len(entries) > MaxPaths {
		return fail("limit", "snapshot path limit")
	}
	var total int64
	seen := map[string]bool{}
	for i, e := range entries {
		if !ValidPath(e.Path) || !ValidDigest(e.Digest) || e.Size < 0 || e.Size > MaxFileBytes {
			return fail("invalid", "invalid snapshot entry")
		}
		if i > 0 && entries[i-1].Path >= e.Path {
			return fail("invalid", "noncanonical or duplicate path")
		}
		for at := strings.LastIndex(e.Path, "/"); at >= 0; at = strings.LastIndex(e.Path[:at], "/") {
			if seen[e.Path[:at]] {
				return fail("conflict", "file/directory path collision")
			}
		}
		seen[e.Path] = true
		total += e.Size
		if total > MaxLogicalBytes {
			return fail("limit", "snapshot byte limit")
		}
	}
	return nil
}
