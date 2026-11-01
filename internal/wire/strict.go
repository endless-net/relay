package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// CheckJSON rejects ambiguous objects before any custom UnmarshalJSON method runs.
func CheckJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("json_invalid_utf8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("json_depth")
		}
		token, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return errors.New("duplicate_json_key")
				}
				seen[k] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return errors.New("json_object")
			}
		case '[':
			for d.More() {
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return errors.New("json_array")
			}
		default:
			return errors.New("json_delimiter")
		}
		return nil
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); !errors.Is(e, io.EOF) {
		return errors.New("json_trailing_value")
	}
	return nil
}

func exactFields(raw []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[time.Time]() {
		return nil
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var object map[string]json.RawMessage
	if e := json.Unmarshal(raw, &object); e != nil {
		return e
	}
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
	for name, body := range object {
		ft, ok := fields[name]
		if !ok {
			return errors.New("unknown_json_field")
		}
		if e := exactFields(body, ft); e != nil {
			return e
		}
		if name == "payload" {
			var encoded string
			if e := json.Unmarshal(body, &encoded); e != nil {
				return e
			}
			decoded, e := base64.StdEncoding.Strict().DecodeString(encoded)
			if e != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
				return errors.New("invalid_payload_encoding")
			}
		}
	}
	return nil
}
