package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthentication(t *testing.T) {
	now := time.Unix(1800000000, 0)
	key := bytes.Repeat([]byte{0x42}, 32)
	c := Claims{1, "agent", "work", "docs/", []string{"read", "write"}, now.Unix(), now.Unix() + 60}
	token, err := Mint(key, c, now)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(key, token, now)
	if err != nil || verified.Sub != "agent" {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"missing": "", "tamper": token[:10] + "x" + token[11:], "version": "BH2" + token[3:], "trailing": token + ".", "oversized": strings.Repeat("a", 4097)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(key, token, now); err == nil {
				t.Fatal("invalid token authenticated")
			}
		})
	}
	if _, err = Verify(key, token, time.Unix(c.Exp, 0)); err == nil {
		t.Fatal("expiry boundary accepted")
	}
	if _, err = Verify(bytes.Repeat([]byte{1}, 32), token, now); err == nil {
		t.Fatal("wrong key accepted")
	}
	for name, change := range map[string]func(*Claims){"long-ttl": func(c *Claims) { c.Exp = c.Iat + 3601 }, "future": func(c *Claims) { c.Iat++; c.Exp++ }, "negative": func(c *Claims) { c.Iat = -1 }, "wildcard-agent": func(c *Claims) { c.Branch = "*" }, "prefix": func(c *Claims) { c.Prefix = "docs" }, "dot-prefix": func(c *Claims) { c.Prefix = "../" }, "duplicate-op": func(c *Claims) { c.Ops = []string{"read", "read"} }, "mixed-admin": func(c *Claims) { c.Ops = []string{"admin", "read"}; c.Branch = "*" }} {
		t.Run(name, func(t *testing.T) {
			v := c
			change(&v)
			if _, err := Mint(key, v, now); err == nil {
				t.Fatal("invalid claims minted")
			}
		})
	}
}
func TestScope(t *testing.T) {
	c := Claims{Branch: "work", Prefix: "docs/", Ops: []string{"read", "write"}}
	for _, v := range []struct {
		op, b, p string
		want     bool
	}{{"read", "work", "docs/a", true}, {"write", "work", "docs/nested/a", true}, {"read", "work", "docs-private/a", false}, {"read", "other", "docs/a", false}, {"admin", "work", "docs/a", false}, {"read", "work", "", true}} {
		if c.Allows(v.op, v.b, v.p) != v.want {
			t.Fatalf("scope mismatch: %+v", v)
		}
	}
}
func TestKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := InitKey(dir); err != nil {
		t.Fatal(err)
	}
	key, err := ReadKey(dir)
	if err != nil || len(key) != 32 {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.key")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("unsafe key permissions")
	}
	if err = InitKey(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := ReadKey(dir)
	if !bytes.Equal(key, again) {
		t.Fatal("key silently rotated")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadKey(dir); err == nil {
		t.Fatal("public key file accepted")
	}
}
func FuzzToken(f *testing.F) {
	for _, v := range []string{"", "BH1.a.b", "BH1..."} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if len(v) > 8192 {
			return
		}
		_, _ = Verify(bytes.Repeat([]byte{42}, 32), v, time.Unix(1800000000, 0))
	})
}
