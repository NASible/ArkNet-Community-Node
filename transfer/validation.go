package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"
)

// exactObject rejects unknown, duplicate (including escaped aliases), missing,
// null, case-aliased fields, trailing values, and oversized bodies before use.
func exactObject(r *http.Request, fields ...string) (map[string]json.RawMessage, error) {
	return exactObjectWithOptional(r, "", fields...)
}

func exactObjectWithOptional(r *http.Request, optional string, fields ...string) (map[string]json.RawMessage, error) {
	return exactObjectWithOptionalFields(r, []string{optional}, fields...)
}

func exactObjectWithOptionalFields(r *http.Request, optional []string, fields ...string) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(data) > 4096 || !utf8.Valid(data) {
		return nil, errBody
	}
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errBody
	}
	out := make(map[string]json.RawMessage, len(fields)+len(optional))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, errBody
		}
		key, ok := token.(string)
		if !ok {
			return nil, errBody
		}
		allowed := false
		for _, field := range optional {
			if key == field && field != "" {
				allowed = true
				break
			}
		}
		for _, field := range fields {
			if key == field {
				allowed = true
				break
			}
		}
		if _, duplicate := out[key]; duplicate || !allowed {
			return nil, errBody
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errBody
		}
		out[key] = raw
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return nil, errBody
	}
	if _, err := d.Token(); err != io.EOF || len(out) < len(fields) || len(out) > len(fields)+len(optional) {
		return nil, errBody
	}
	for _, field := range fields {
		if _, ok := out[field]; !ok {
			return nil, errBody
		}
	}
	return out, nil
}

var errBody = &bodyError{}

type bodyError struct{}

func (*bodyError) Error() string { return "invalid_request" }
