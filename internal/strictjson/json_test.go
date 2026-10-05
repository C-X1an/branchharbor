package strictjson

import (
	"strings"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	type Value struct {
		Name   string            `json:"name"`
		Nested map[string]string `json:"nested"`
	}
	for _, s := range []string{`{"name":"a","name":"b"}`, `{"Name":"a"}`, `{"name":"a","extra":1}`, `{"name":"a"} {}`, `{"nested":{"a":"b","\u0061":"c"}}`, "{\"name\":\"\xff\"}", strings.Repeat("[", 40) + strings.Repeat("]", 40)} {
		var v Value
		if err := Decode([]byte(s), &v); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	var v Value
	if err := Decode([]byte(`{"name":"ok","nested":{"a":"b"}}`), &v); err != nil || v.Name != "ok" {
		t.Fatal(err)
	}
}
func FuzzStrictJSON(f *testing.F) {
	for _, s := range []string{`{}`, `{"name":"x"}`, `null`, `{"name":"a","name":"b"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 65536 {
			return
		}
		var v struct {
			Name string `json:"name"`
		}
		_ = Decode([]byte(s), &v)
	})
}
