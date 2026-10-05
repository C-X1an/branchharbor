package api

import (
	"branchharbor/internal/auth"
	"branchharbor/internal/store"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	s            *store.Store
	h            *Handler
	server       *httptest.Server
	admin, agent string
}

func setup(t *testing.T) fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	head, err := s.Head("main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Fork("operator", store.ForkRequest{Branch: "work", Source: "main", Expected: head, RequestID: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	now := time.Now()
	admin, err := auth.Mint(key, auth.Claims{Schema: 1, Sub: "operator", Branch: "*", Ops: []string{"admin"}, Iat: now.Unix(), Exp: now.Unix() + 900}, now)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := auth.Mint(key, auth.Claims{Schema: 1, Sub: "agent", Branch: "work", Prefix: "docs/", Ops: []string{"read", "write"}, Iat: now.Unix(), Exp: now.Unix() + 900}, now)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, key)
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return fixture{s, h, server, admin, agent}
}
func (f fixture) request(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	switch b := body.(type) {
	case string:
		data = []byte(b)
	case nil:
	default:
		var err error
		data, err = json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing response headers")
	}
	return resp.StatusCode, b
}
func TestAuthenticationHTTP(t *testing.T) {
	f := setup(t)
	for _, token := range []string{"", "bad-token", f.agent + "tamper"} {
		status, _ := f.request(t, "GET", "/v1/head?branch=work", token, nil)
		if status != 401 {
			t.Fatalf("invalid token got %d", status)
		}
	}
	status, b := f.request(t, "GET", "/v1/head?branch=work", f.agent, nil)
	if status != 200 {
		t.Fatalf("valid token got %d %s", status, b)
	}
	f.h.now = func() time.Time { return time.Now().Add(time.Hour) }
	status, _ = f.request(t, "GET", "/v1/head?branch=work", f.agent, nil)
	if status != 401 {
		t.Fatal("expired HTTP token accepted")
	}
}
func TestScopeHTTP(t *testing.T) {
	f := setup(t)
	h, _ := f.s.Head("work")
	req := store.CommitRequest{Branch: "work", Expected: h, RequestID: "one", Puts: map[string][]byte{"docs/a": []byte("public-to-agent")}}
	status, b := f.request(t, "POST", "/v1/commit", f.agent, req)
	if status != 200 {
		t.Fatal(status, string(b))
	}
	for _, path := range []string{"/v1/head?branch=main", "/v1/head?branch=nonexistent", "/v1/file?branch=work&path=docs-private/a", "/v1/file?branch=main&path=docs/a"} {
		status, _ := f.request(t, "GET", path, f.agent, nil)
		if status != 403 {
			t.Fatalf("scope bypass %s %d", path, status)
		}
	}
	for _, p := range []string{"docs-private/a", "elsewhere/a"} {
		req.RequestID = p
		req.Puts = map[string][]byte{p: []byte("bad")}
		status, _ := f.request(t, "POST", "/v1/commit", f.agent, req)
		if status != 403 {
			t.Fatal("write escaped prefix", status)
		}
	}
	h, _ = f.s.Head("work")
	_, err := f.s.Commit("operator", store.CommitRequest{Branch: "work", Expected: h, RequestID: "hidden", Puts: map[string][]byte{"private/a": []byte("TOP-SECRET-FIXTURE")}})
	if err != nil {
		t.Fatal(err)
	}
	status, b = f.request(t, "GET", "/v1/files?branch=work", f.agent, nil)
	if status != 200 || bytes.Contains(b, []byte("private/a")) {
		t.Fatal("listing leaked path", string(b))
	}
	// Retrying an old authorized operation with an unauthorized token must still
	// fail at the HTTP boundary, before the engine's idempotency table is checked.
	status, _ = f.request(t, "POST", "/v1/commit", "invalid", store.CommitRequest{Branch: "work", Expected: h, RequestID: "one", Puts: map[string][]byte{"docs/a": []byte("public-to-agent")}})
	if status != 401 {
		t.Fatal("retry bypassed auth")
	}
}
func TestAdminBoundary(t *testing.T) {
	f := setup(t)
	for _, route := range []string{"/v1/branches", "/metrics", "/v1/fork", "/v1/merge", "/v1/merge/preview", "/v1/restore"} {
		method := "POST"
		var body any = map[string]any{}
		if route == "/v1/branches" || route == "/metrics" {
			method = "GET"
			body = nil
		}
		status, _ := f.request(t, method, route, f.agent, body)
		if status != 403 {
			t.Fatalf("agent reached admin route %s: %d", route, status)
		}
	}
	status, _ := f.request(t, "GET", "/v1/branches", f.admin, nil)
	if status != 200 {
		t.Fatal("admin denied")
	}
}
func TestPathsHTTP(t *testing.T) {
	f := setup(t)
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("UNCHANGED"), 0600); err != nil {
		t.Fatal(err)
	}
	h, _ := f.s.Head("work")
	for i, p := range []string{"../sentinel", sentinel, "docs//a", "docs/../a", "docs/%2e%2e/a", "docs\\a"} {
		status, _ := f.request(t, "POST", "/v1/commit", f.admin, store.CommitRequest{Branch: "work", Expected: h, RequestID: string(rune('a' + i)), Puts: map[string][]byte{p: []byte("ATTACK")}})
		if status != 400 {
			t.Fatalf("path %q returned %d", p, status)
		}
		status, _ = f.request(t, "GET", "/v1/file?branch=work&path="+url.QueryEscape(p), f.admin, nil)
		if status != 400 {
			t.Fatalf("read path %q returned %d", p, status)
		}
	}
	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "UNCHANGED" {
		t.Fatal("host file changed")
	}
}
func TestStrictRequests(t *testing.T) {
	f := setup(t)
	for _, b := range []string{`{"branch":"work","branch":"main"}`, `{"Branch":"work"}`, `{"actor":"operator"}`, `{} {}`, `{"puts":{"docs/a":"YQ==","docs/a":"Yg=="}}`} {
		status, _ := f.request(t, "POST", "/v1/commit", f.admin, b)
		if status != 400 {
			t.Fatal("ambiguous request accepted", status, b)
		}
	}
	for _, p := range []string{"/v1/head?branch=work&branch=main", "/v1/head?branch=work&extra=1", "/v1/head?branch=%xx", "/v1/head", "/v1/branches?extra=1"} {
		status, _ := f.request(t, "GET", p, f.admin, nil)
		if status != 400 {
			t.Fatal("query accepted", status, p)
		}
	}
	status, _ := f.request(t, "POST", "/v1/fork?extra=1", f.admin, "{}")
	if status != 400 {
		t.Fatal("POST query accepted")
	}
}
func TestLimitsHTTP(t *testing.T) {
	f := setup(t)
	body := `{"x":"` + strings.Repeat("x", MaxBody) + `"}`
	status, _ := f.request(t, "POST", "/v1/commit", f.admin, body)
	if status != 413 {
		t.Fatal("body size not enforced", status)
	}
	status, _ = f.request(t, "GET", "/missing", f.admin, nil)
	if status != 404 {
		t.Fatal(status)
	}
	status, _ = f.request(t, "POST", "/v1/head", f.admin, "{}")
	if status != 405 {
		t.Fatal(status)
	}
}
func TestAdmission(t *testing.T) {
	f := setup(t)
	for i := 0; i < AdmissionLimit; i++ {
		f.h.slots <- struct{}{}
	}
	status, _ := f.request(t, "GET", "/healthz", "", nil)
	if status != 429 {
		t.Fatal("admission cap not enforced", status)
	}
	for i := 0; i < AdmissionLimit; i++ {
		<-f.h.slots
	}
	status, _ = f.request(t, "GET", "/healthz", "", nil)
	if status != 200 {
		t.Fatal("admission not released")
	}
}
func TestSafeDefaults(t *testing.T) {
	f := setup(t)
	req := httptest.NewRequest("GET", "/v1/head?branch=work", nil)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Authorization", "Bearer "+f.admin)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("browser origin accepted")
	}
	if strings.Contains(w.Body.String(), f.admin) {
		t.Fatal("token leaked in response")
	}
	for _, address := range []string{"0.0.0.0:0", ":0", "localhost:0", "[::]:0", "192.0.2.1:0"} {
		ln, err := Listen(address)
		if err == nil {
			ln.Close()
			t.Fatal("non-explicit-loopback address accepted", address)
		}
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	server := Server(f.h)
	if server.ReadHeaderTimeout == 0 || server.ReadTimeout == 0 || server.WriteTimeout == 0 || server.MaxHeaderBytes != 16<<10 {
		t.Fatal("missing server bounds")
	}
}
