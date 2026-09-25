package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/up2jj/wuko/plugin/sdk"
)

const (
	namespace    = "cue"
	stepType     = namespace + ".eval"
	maxFrameSize = 10 << 20
)

type stepContext struct {
	StepID       string         `json:"step_id"`
	WorkflowName string         `json:"workflow_name"`
	WorkflowDir  string         `json:"workflow_dir"`
	RunDir       string         `json:"run_dir"`
	Vars         map[string]any `json:"vars"`
	Inputs       map[string]any `json:"inputs"`
	Env          map[string]any `json:"env"`
	Steps        map[string]any `json:"steps"`
	Dependencies map[string]any `json:"dependencies"`
	Attempt      int            `json:"attempt"`
	MaxAttempts  int            `json:"max_attempts"`
	OperationID  string         `json:"operation_id"`
}

type workerFunc func(context.Context, workerJob) (json.RawMessage, error)

func newPlugin(worker workerFunc) (*sdk.Plugin, error) {
	plugin, err := sdk.New(namespace)
	if err != nil {
		return nil, err
	}
	err = sdk.RegisterStep(plugin, "eval", sdk.StepHandler[json.RawMessage]{
		Validate: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) error {
			configuration, err := decodeConfig(request.Config)
			if err != nil {
				return pluginError(err)
			}
			if configuration.validationMustBeDeferred() {
				return nil
			}
			stepContext := legacyStepContext(request.Context)
			_, err = worker(ctx, workerJob{Mode: workerModeValidate, Configuration: configuration, Context: stepContext})
			return pluginError(redactError(err, stepContext.Env))
		},
		Run: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) (sdk.Result, error) {
			configuration, err := decodeConfig(request.Config)
			if err != nil {
				return sdk.Result{}, pluginError(err)
			}
			stepContext := legacyStepContext(request.Context)
			value, err := worker(ctx, workerJob{Mode: workerModeRun, Configuration: configuration, Context: stepContext})
			if err != nil {
				return sdk.Result{}, pluginError(redactError(err, stepContext.Env))
			}
			return sdk.Result{Outputs: map[string]any{"value": value}}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return plugin, nil
}

func legacyStepContext(value sdk.StepContext) stepContext {
	environment := make(map[string]any, len(value.Env))
	for name, item := range value.Env {
		environment[name] = item
	}
	dependencies := make(map[string]any, len(value.Dependencies))
	for name, item := range value.Dependencies {
		dependencies[name] = item
	}
	return stepContext{
		StepID: value.StepID, WorkflowName: value.WorkflowName, WorkflowDir: value.WorkflowDir, RunDir: value.RunDir,
		Vars: value.Vars, Inputs: value.Inputs, Env: environment, Steps: value.Steps, Dependencies: dependencies,
		Attempt: value.Attempt, MaxAttempts: value.MaxAttempts, OperationID: value.OperationID,
	}
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("multiple JSON values are not supported")
	}
	return nil
}

func decodeRawStrict(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeStrict(data, target)
}

func pluginError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sdk.Coded("canceled", err)
	}
	return sdk.Coded("evaluation_failed", err)
}
