package transfer

import (
	"bytes"
	"encoding/json"
	"io"
)

type listResponse struct {
	Transfers []Status `json:"transfers"`
}

type seedListResponse struct {
	Seeds []SeedStatus `json:"seeds"`
}

func seedShape(data []byte) bool { return completeShape(data, "object_id", "state") }

// completeShape requires every wire field, including zero-valued fields.
func completeShape(data []byte, fields ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || len(object) != len(fields) {
		return false
	}
	for _, field := range fields {
		raw, ok := object[field]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	return true
}
func statusShape(data []byte) bool {
	return completeShape(data, "object_id", "state", "total_bytes", "completed_bytes", "mock", "error_code")
}
func responseShape(data []byte, output any) bool {
	switch output.(type) {
	case *Status:
		return statusShape(data)
	case *listResponse:
		if !completeShape(data, "transfers") {
			return false
		}
		var object struct {
			Transfers []json.RawMessage `json:"transfers"`
		}
		if json.Unmarshal(data, &object) != nil || object.Transfers == nil {
			return false
		}
		for _, item := range object.Transfers {
			if !statusShape(item) {
				return false
			}
		}
		return true
	case *SeedStatus:
		return seedShape(data)
	case *seedListResponse:
		if !completeShape(data, "seeds") {
			return false
		}
		var object struct {
			Seeds []json.RawMessage `json:"seeds"`
		}
		if json.Unmarshal(data, &object) != nil || object.Seeds == nil {
			return false
		}
		for _, item := range object.Seeds {
			if !seedShape(item) {
				return false
			}
		}
		return true
	default:
		return completeShape(data, "error")
	}
}

// uniqueJSON rejects duplicate members, which encoding/json otherwise silently overwrites.
func uniqueJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if !uniqueValue(d) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
func uniqueValue(d *json.Decoder) bool {
	token, err := d.Token()
	if err != nil || token == nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !uniqueValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !uniqueValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}
