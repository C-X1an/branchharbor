// Package strictjson rejects ambiguous JSON before typed decoding.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Decode rejects duplicate keys, case aliases, unknown fields, excess nesting,
// trailing values, and invalid UTF-8. Callers must independently bound byte size.
func Decode(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := value(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	if err := exactFields(generic, reflect.TypeOf(dst)); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func value(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting limit")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate or invalid JSON key")
			}
			seen[s] = true
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case json.Delim('['):
		for d.More() {
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	case json.Delim('}'), json.Delim(']'):
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
func exactFields(v any, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		} // typed decoder checks mismatched types
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		for k, x := range m {
			ft, ok := fields[k]
			if !ok {
				return errors.New("unknown or noncanonical JSON field")
			}
			if err := exactFields(x, ft); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if xs, ok := v.([]any); ok {
			for _, x := range xs {
				if err := exactFields(x, t.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if m, ok := v.(map[string]any); ok {
			for _, x := range m {
				if err := exactFields(x, t.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
