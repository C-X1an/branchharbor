// Package auth implements a local operator-minted bearer capability. It is not
// OAuth, delegation, or authentication for arbitrary remote deployments.
package auth

import (
	"branchharbor/internal/store"
	"branchharbor/internal/strictjson"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Claims struct {
	Schema int      `json:"schema"`
	Sub    string   `json:"sub"`
	Branch string   `json:"branch"`
	Prefix string   `json:"prefix"`
	Ops    []string `json:"ops"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
}

var ErrUnauthorized = errors.New("invalid authorization")

func (c Claims) Admin() bool {
	for _, op := range c.Ops {
		if op == "admin" {
			return true
		}
	}
	return false
}
func (c Claims) Allows(op, branch, path string) bool {
	if c.Admin() {
		return true
	}
	if c.Branch != branch {
		return false
	}
	found := false
	for _, v := range c.Ops {
		if v == op {
			found = true
		}
	}
	return found && (path == "" || strings.HasPrefix(path, c.Prefix))
}
func (c Claims) Validate(now time.Time) error {
	if c.Schema != 1 || !store.ValidID(c.Sub) || len(c.Ops) == 0 || len(c.Ops) > 3 || c.Iat < 0 || c.Exp <= c.Iat || c.Exp-c.Iat > 3600 || c.Iat > now.Unix() || now.Unix() >= c.Exp {
		return ErrUnauthorized
	}
	seen := map[string]bool{}
	for _, op := range c.Ops {
		if seen[op] || (op != "read" && op != "write" && op != "admin") {
			return ErrUnauthorized
		}
		seen[op] = true
	}
	if c.Admin() {
		if c.Branch != "*" || c.Prefix != "" || len(c.Ops) != 1 {
			return ErrUnauthorized
		}
	} else if !store.ValidBranch(c.Branch) {
		return ErrUnauthorized
	}
	if c.Prefix != "" && (!strings.HasSuffix(c.Prefix, "/") || !store.ValidPath(strings.TrimSuffix(c.Prefix, "/"))) {
		return ErrUnauthorized
	}
	return nil
}
func Mint(key []byte, c Claims, now time.Time) (string, error) {
	if len(key) != 32 {
		return "", ErrUnauthorized
	}
	if err := c.Validate(now); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	prefix := "BH1." + payload
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(prefix))
	return prefix + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func Verify(key []byte, token string, now time.Time) (Claims, error) {
	var c Claims
	if len(key) != 32 || len(token) > 4096 {
		return c, ErrUnauthorized
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "BH1" {
		return c, ErrUnauthorized
	}
	macBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return c, ErrUnauthorized
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), macBytes) {
		return c, ErrUnauthorized
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return c, ErrUnauthorized
	}
	if err = strictjson.Decode(payload, &c); err != nil {
		return Claims{}, ErrUnauthorized
	}
	if err = c.Validate(now); err != nil {
		return Claims{}, err
	}
	return c, nil
}
func privateRead(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("credential permissions must be owner-only")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	if len(b) > 4096 {
		return nil, errors.New("credential file too large")
	}
	return b, nil
}
func ReadKey(dir string) ([]byte, error) {
	b, err := privateRead(filepath.Join(dir, "auth.key"))
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, errors.New("invalid key length")
	}
	return b, nil
}

// InitKey uses O_EXCL and never replaces an existing key. Call only while owning
// the store lock; rotations deliberately require operator intervention.
func InitKey(dir string) error {
	if _, err := ReadKey(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	return WritePrivate(filepath.Join(dir, "auth.key"), key)
}
func WritePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}
