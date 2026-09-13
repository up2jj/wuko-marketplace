package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const maxSourceSize = 1 << 20

type config struct {
	Source  string `json:"source"`
	File    string `json:"file"`
	Package string `json:"package"`
}

func decodeConfig(raw json.RawMessage) (config, error) {
	var result config
	if err := decodeRawStrict(raw, &result); err != nil {
		return config{}, fmt.Errorf("invalid cue.eval configuration: %w", err)
	}
	configured := 0
	for _, value := range []string{result.Source, result.File, result.Package} {
		if value != "" {
			configured++
		}
	}
	if configured != 1 {
		return config{}, fmt.Errorf("invalid cue.eval configuration: exactly one of source, file or package is required")
	}
	return result, nil
}

func (c config) validationMustBeDeferred() bool {
	if c.Source != "" {
		return strings.Contains(c.Source, "{{")
	}
	if c.File != "" {
		return strings.Contains(c.File, "{{")
	}
	return strings.Contains(c.Package, "{{")
}

func redactError(err error, environment map[string]any) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := err.Error()
	values := make([]string, 0, len(environment))
	for _, value := range environment {
		text, ok := value.(string)
		if ok && text != "" {
			values = append(values, text)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		message = strings.ReplaceAll(message, strconv.Quote(value), `"[REDACTED]"`)
		if len(value) >= 8 {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	return errors.New(message)
}
