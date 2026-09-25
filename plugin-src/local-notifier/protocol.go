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
	namespace = "local-notifier"
	stepType  = namespace + ".notify"
)

func newPlugin(notify notifyFunc) (*sdk.Plugin, error) {
	plugin, err := sdk.New(namespace)
	if err != nil {
		return nil, err
	}
	err = sdk.RegisterStep(plugin, "notify", sdk.StepHandler[json.RawMessage]{
		Validate: func(_ context.Context, request sdk.StepRequest[json.RawMessage]) error {
			_, err := decodeConfig(request.Config, true)
			return pluginError("notification_failed", err)
		},
		Run: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) (sdk.Result, error) {
			configuration, err := decodeConfig(request.Config, false)
			if err != nil {
				return sdk.Result{}, pluginError("notification_failed", err)
			}
			result, err := notify(ctx, configuration, request.Context.WorkflowName, func(data []byte) error {
				_, err := request.Stdout.Write(data)
				return err
			})
			if err != nil {
				return sdk.Result{}, pluginError("notification_failed", err)
			}
			return sdk.Result{Outputs: map[string]any{
				"delivery": result.Delivery,
				"fallback": result.Fallback,
			}}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return plugin, nil
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

func decodeRawStrict(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
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
