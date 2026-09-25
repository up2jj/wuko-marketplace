package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/up2jj/wuko/plugin/sdk"
)

const namespace = "jwt"

func newPlugin(service *service) (*sdk.Plugin, error) {
	plugin, err := sdk.New(namespace)
	if err != nil {
		return nil, err
	}
	adapter := &pluginAdapter{service: service}
	for _, name := range []string{"generate", "jwks", "verify", "is_valid"} {
		if err := plugin.RegisterHelper(name, func(ctx context.Context, args []any) (any, error) {
			raw, err := rawArguments(args)
			if err != nil {
				return nil, pluginError("operation_failed", err)
			}
			value, err := adapter.callHelper(ctx, helperParams{Name: name, Args: raw})
			return value, pluginError("operation_failed", err)
		}); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"generate", "jwks", "verify", "is_valid"} {
		stepType := namespace + "." + name
		err := sdk.RegisterStep(plugin, name, sdk.StepHandler[json.RawMessage]{
			Validate: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) error {
				_, err := adapter.callStep(ctx, stepType, request.Config, true)
				return pluginError("operation_failed", err)
			},
			Run: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) (sdk.Result, error) {
				outputs, err := adapter.callStep(ctx, stepType, request.Config, false)
				return sdk.Result{Outputs: outputs}, pluginError("operation_failed", err)
			},
		})
		if err != nil {
			return nil, err
		}
	}
	return plugin, nil
}

func rawArguments(args []any) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, len(args))
	for index, arg := range args {
		data, err := json.Marshal(arg)
		if err != nil {
			return nil, fmt.Errorf("encoding helper argument %d: %w", index+1, err)
		}
		result[index] = data
	}
	return result, nil
}

func pluginError(code string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sdk.Coded("canceled", err)
	}
	return sdk.Coded(code, err)
}
