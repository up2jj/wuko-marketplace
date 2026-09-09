package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	workerArgument     = "__cue_eval_worker"
	workerModeValidate = "validate"
	workerModeRun      = "run"
)

type workerJob struct {
	Mode     string      `json:"mode"`
	Source   string      `json:"source"`
	Filename string      `json:"filename"`
	Context  stepContext `json:"context"`
}

type workerResponse struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

func externalWorker(executable string) workerFunc {
	return func(ctx context.Context, job workerJob) (json.RawMessage, error) {
		payload, err := json.Marshal(job)
		if err != nil {
			return nil, fmt.Errorf("encoding CUE worker request: %w", err)
		}
		command := exec.CommandContext(ctx, executable, workerArgument)
		command.Env = []string{}
		command.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("CUE worker failed: %s", detail)
		}
		var response workerResponse
		if err := decodeStrict(stdout.Bytes(), &response); err != nil {
			return nil, fmt.Errorf("decoding CUE worker response: %w", err)
		}
		if response.Error != "" {
			return nil, errors.New(response.Error)
		}
		return response.Value, nil
	}
}

func serveWorker(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, maxFrameSize+1))
	if err != nil {
		return fmt.Errorf("reading CUE worker request: %w", err)
	}
	if len(data) > maxFrameSize {
		return fmt.Errorf("CUE worker request exceeds the 10 MiB limit")
	}
	var job workerJob
	if err := decodeStrict(data, &job); err != nil {
		return fmt.Errorf("decoding CUE worker request: %w", err)
	}
	if job.Mode != workerModeValidate && job.Mode != workerModeRun {
		return fmt.Errorf("unknown CUE worker mode %q", job.Mode)
	}
	value, evaluationErr := evaluate(job)
	response := workerResponse{Value: value}
	if evaluationErr != nil {
		response.Value = nil
		response.Error = evaluationErr.Error()
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return fmt.Errorf("encoding CUE worker response: %w", err)
	}
	return nil
}
