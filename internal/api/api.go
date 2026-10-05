// Package api is the untrusted-client boundary. No route executes code,
// materializes logical paths, fetches URLs, or exposes raw object digests.
package api

import (
	"branchharbor/internal/auth"
	"branchharbor/internal/store"
	"branchharbor/internal/strictjson"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const MaxBody = 8 << 20
const AdmissionLimit = 16

type Handler struct {
	s                         *store.Store
	key                       []byte
	slots                     chan struct{}
	requests, errs, mutations atomic.Uint64
	now                       func() time.Time
}

func New(s *store.Store, key []byte) *Handler {
	return &Handler{s: s, key: append([]byte{}, key...), slots: make(chan struct{}, AdmissionLimit), now: time.Now}
}
func Server(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
}
func Listen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("only explicit loopback addresses are supported")
	}
	return net.Listen("tcp", address)
}
func (h *Handler) respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func (h *Handler) error(w http.ResponseWriter, status int, code string) {
	h.errs.Add(1)
	h.respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
func (h *Handler) result(w http.ResponseWriter, data any, err error) {
	if err == nil {
		h.respond(w, 200, data)
		return
	}
	code := store.Code(err)
	status := map[string]int{"invalid": 400, "not_found": 404, "conflict": 409, "idempotency_conflict": 409, "ambiguous_base": 409, "limit": 413, "unavailable": 503}[code]
	if status == 0 {
		status = 500
		code = "internal"
	}
	h.error(w, status, code)
}
func query(r *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	a := map[string]bool{}
	for _, v := range allowed {
		a[v] = true
	}
	for k, v := range q {
		if !a[k] || len(v) != 1 {
			return nil, errors.New("invalid query")
		}
	}
	for _, k := range allowed {
		if len(q[k]) != 1 {
			return nil, errors.New("missing query")
		}
	}
	return q, nil
}
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.error(w, 400, "invalid")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			h.error(w, 413, "limit")
		} else {
			h.error(w, 400, "invalid")
		}
		return false
	}
	if err = strictjson.Decode(b, v); err != nil {
		h.error(w, 400, "invalid")
		return false
	}
	return true
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.requests.Add(1)
	if _, ok := r.Header["Origin"]; ok {
		h.error(w, 403, "forbidden")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.error(w, 429, "busy")
		return
	}
	// Reject path encodings/normalization ambiguities; logical file paths appear
	// only in validated single-valued queries or strict JSON fields.
	if r.URL.RawPath != "" || strings.Contains(r.URL.Path, "//") {
		h.error(w, 400, "invalid")
		return
	}
	routes := map[string]string{"/healthz": "GET", "/v1/head": "GET", "/v1/files": "GET", "/v1/file": "GET", "/v1/branches": "GET", "/metrics": "GET", "/v1/commit": "POST", "/v1/fork": "POST", "/v1/merge/preview": "POST", "/v1/merge": "POST", "/v1/restore": "POST"}
	method, ok := routes[r.URL.Path]
	if !ok {
		h.error(w, 404, "not_found")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		h.error(w, 405, "invalid")
		return
	}
	if r.URL.Path == "/healthz" {
		if _, err := query(r); err != nil {
			h.error(w, 400, "invalid")
			return
		}
		if !h.s.Ready() {
			h.error(w, 503, "unavailable")
		} else {
			h.respond(w, 200, map[string]bool{"ready": true})
		}
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		h.error(w, 401, "unauthenticated")
		return
	}
	c, err := auth.Verify(h.key, strings.TrimPrefix(values[0], "Bearer "), h.now())
	if err != nil {
		h.error(w, 401, "unauthenticated")
		return
	}
	adminRoute := r.URL.Path == "/v1/branches" || r.URL.Path == "/metrics" || r.URL.Path == "/v1/fork" || strings.HasPrefix(r.URL.Path, "/v1/merge") || r.URL.Path == "/v1/restore"
	if adminRoute && !c.Admin() {
		h.error(w, 403, "forbidden")
		return
	}
	if method == "POST" && r.URL.RawQuery != "" {
		h.error(w, 400, "invalid")
		return
	}
	switch r.URL.Path {
	case "/v1/head", "/v1/files", "/v1/file":
		keys := []string{"branch"}
		if r.URL.Path == "/v1/file" {
			keys = append(keys, "path")
		}
		q, err := query(r, keys...)
		if err != nil {
			h.error(w, 400, "invalid")
			return
		}
		branch, path := q.Get("branch"), q.Get("path")
		if !c.Allows("read", branch, path) {
			h.error(w, 403, "forbidden")
			return
		}
		if !store.ValidBranch(branch) || (r.URL.Path == "/v1/file" && !store.ValidPath(path)) {
			h.error(w, 400, "invalid")
			return
		}
		switch r.URL.Path {
		case "/v1/head":
			head, err := h.s.Head(branch)
			h.result(w, head, err)
		case "/v1/file":
			head, b, err := h.s.Read(branch, path)
			h.result(w, struct {
				Head store.Head `json:"head"`
				Data []byte     `json:"data"`
			}{head, b}, err)
		case "/v1/files":
			head, files, err := h.s.Snapshot(branch)
			filtered := []store.Entry{}
			for _, e := range files {
				if c.Allows("read", branch, e.Path) {
					filtered = append(filtered, e)
				}
			}
			h.result(w, struct {
				Head  store.Head    `json:"head"`
				Files []store.Entry `json:"files"`
			}{head, filtered}, err)
		}
	case "/v1/branches":
		if _, err := query(r); err != nil {
			h.error(w, 400, "invalid")
			return
		}
		b, err := h.s.Branches()
		h.result(w, map[string]any{"branches": b}, err)
	case "/metrics":
		if _, err := query(r); err != nil {
			h.error(w, 400, "invalid")
			return
		}
		h.respond(w, 200, map[string]uint64{"requests": h.requests.Load(), "errors": h.errs.Load(), "mutations": h.mutations.Load()})
	case "/v1/commit":
		var req store.CommitRequest
		if !h.decode(w, r, &req) {
			return
		}
		if !c.Allows("write", req.Branch, "") {
			h.error(w, 403, "forbidden")
			return
		}
		for p := range req.Puts {
			if !c.Allows("write", req.Branch, p) {
				h.error(w, 403, "forbidden")
				return
			}
		}
		for _, p := range req.Deletes {
			if !c.Allows("write", req.Branch, p) {
				h.error(w, 403, "forbidden")
				return
			}
		}
		result, err := h.s.Commit(c.Sub, req)
		if err == nil && !result.Replayed {
			h.mutations.Add(1)
		}
		h.result(w, result, err)
	case "/v1/fork":
		var req store.ForkRequest
		if !h.decode(w, r, &req) {
			return
		}
		result, err := h.s.Fork(c.Sub, req)
		if err == nil && !result.Replayed {
			h.mutations.Add(1)
		}
		h.result(w, result, err)
	case "/v1/merge/preview":
		var req struct {
			Target string `json:"target"`
			Source string `json:"source"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		result, err := h.s.Preview(req.Target, req.Source)
		h.result(w, result, err)
	case "/v1/merge":
		var req store.MergeRequest
		if !h.decode(w, r, &req) {
			return
		}
		result, err := h.s.Merge(c.Sub, req)
		if err == nil && !result.Replayed {
			h.mutations.Add(1)
		}
		h.result(w, result, err)
	case "/v1/restore":
		var req store.RestoreRequest
		if !h.decode(w, r, &req) {
			return
		}
		result, err := h.s.Restore(c.Sub, req)
		if err == nil && !result.Replayed {
			h.mutations.Add(1)
		}
		h.result(w, result, err)
	}
}
