package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// decodeJSONObject decodes one configuration object, rejecting duplicate keys,
// unknown fields, and trailing values. Callers must redact errors before display.
func decodeJSONObject(r io.Reader, destination any) error {
	contents, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(contents)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("expected a JSON object")
	}
	validation := json.NewDecoder(bytes.NewReader(contents))
	if err := validateJSONValue(validation); err != nil {
		return err
	}
	if _, err := validation.Token(); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

// foldJSONKey maps a key to a canonical form under Unicode simple case folding.
// encoding/json matches struct fields case-insensitively, so keys that differ
// only by case would otherwise bypass duplicate detection and the last would win.
func foldJSONKey(key string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < smallest {
				smallest = folded
			}
		}
		return smallest
	}, key)
}

// validateJSONValue consumes one JSON value and rejects repeated object keys at
// every depth, including keys spelled with equivalent JSON escape sequences or
// differing only by letter case.
func validateJSONValue(decoder *json.Decoder) error {
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
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("expected an object key")
			}
			folded := foldJSONKey(key)
			if seen[folded] {
				return errors.New("duplicate JSON object key")
			}
			seen[folded] = true
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
