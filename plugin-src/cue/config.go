package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxSourceSize = 1 << 20

type config struct {
	Source string `json:"source"`
	File   string `json:"file"`
}

func decodeConfig(raw json.RawMessage) (config, error) {
	var result config
	if err := decodeRawStrict(raw, &result); err != nil {
		return config{}, fmt.Errorf("invalid cue.eval configuration: %w", err)
	}
	hasSource := result.Source != ""
	hasFile := result.File != ""
	if hasSource == hasFile {
		return config{}, fmt.Errorf("invalid cue.eval configuration: exactly one of source or file is required")
	}
	return result, nil
}

func (c config) validationMustBeDeferred() bool {
	if c.Source != "" {
		return strings.Contains(c.Source, "{{")
	}
	return strings.Contains(c.File, "{{")
}

func loadSource(configuration config, workflowDir string) (string, string, error) {
	if configuration.Source != "" {
		if len(configuration.Source) > maxSourceSize {
			return "", "", fmt.Errorf("inline CUE source exceeds the 1 MiB limit")
		}
		return configuration.Source, "inline.cue", nil
	}
	if workflowDir == "" {
		return "", "", fmt.Errorf("workflow directory is required for file-based CUE source")
	}
	if filepath.IsAbs(configuration.File) {
		return "", "", fmt.Errorf("CUE file must be relative to the workflow directory")
	}
	if filepath.Ext(configuration.File) != ".cue" {
		return "", "", fmt.Errorf("CUE file must use the .cue extension")
	}
	for _, part := range strings.Split(filepath.ToSlash(configuration.File), "/") {
		if part == ".." {
			return "", "", fmt.Errorf("CUE file path must not contain parent traversal")
		}
	}

	root, err := filepath.Abs(workflowDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving workflow directory: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolving workflow directory: %w", err)
	}
	candidate := filepath.Join(root, filepath.Clean(configuration.File))
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolving CUE file %q: %w", configuration.File, err)
	}
	contained, err := pathContained(realRoot, realCandidate)
	if err != nil {
		return "", "", fmt.Errorf("checking CUE file %q: %w", configuration.File, err)
	}
	if !contained {
		return "", "", fmt.Errorf("CUE file %q resolves outside the workflow directory", configuration.File)
	}
	file, err := os.Open(realCandidate)
	if err != nil {
		return "", "", fmt.Errorf("opening CUE file %q: %w", configuration.File, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "", fmt.Errorf("inspecting CUE file %q: %w", configuration.File, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("CUE file %q is not a regular file", configuration.File)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSourceSize+1))
	if err != nil {
		return "", "", fmt.Errorf("reading CUE file %q: %w", configuration.File, err)
	}
	if len(data) > maxSourceSize {
		return "", "", fmt.Errorf("CUE file %q exceeds the 1 MiB limit", configuration.File)
	}
	return string(data), filepath.ToSlash(configuration.File), nil
}

func pathContained(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
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
