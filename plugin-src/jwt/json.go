package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	maxDocumentSize = 1 << 20
	maxJSONDepth    = 64
)

func decodeStrict(data []byte, target any) error {
	if len(data) > maxDocumentSize {
		return fmt.Errorf("JSON document exceeds 1 MiB")
	}
	if err := validateJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple JSON values are not supported")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func decodeRawStrict(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeStrict(data, target)
}

func decodeObject(data []byte) (map[string]any, error) {
	if len(data) > maxDocumentSize {
		return nil, fmt.Errorf("JSON document exceeds 1 MiB")
	}
	if err := validateJSON(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return value, nil
}

// decodeObjectOrString decodes a JSON object, also accepting a JSON string that holds one,
// because templated step values always render as strings.
func decodeObjectOrString(data []byte) (map[string]any, error) {
	if len(data) > 0 && data[0] == '"' {
		var encoded string
		if err := decodeStrict(data, &encoded); err != nil {
			return nil, err
		}
		data = []byte(encoded)
	}
	return decodeObject(data)
}

func decodeJSON(data []byte, target any) error {
	if len(data) > maxDocumentSize {
		return fmt.Errorf("JSON document exceeds 1 MiB")
	}
	if err := validateJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func validateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == nil {
		return fmt.Errorf("multiple JSON values are not supported")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON object member %q", key)
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("invalid JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
