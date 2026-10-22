package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
	"github.com/up2jj/wuko/plugin/sdk"
	"k8s.io/client-go/kubernetes"
)

type clientFactory func(execution.Options) (kubernetes.Interface, error)

func newPlugin(factory clientFactory) (*sdk.Plugin, error) {
	p, err := sdk.New("kube")
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"run", "list", "status", "watch", "logs", "report", "cancel", "purge"} {
		err = sdk.RegisterStep(p, op, sdk.StepHandler[json.RawMessage]{
			Validate: func(_ context.Context, r sdk.ValidateRequest[json.RawMessage]) error {
				return sdk.Coded("invalid_configuration", execution.ValidateConfiguration(r.Config, op))
			},
			Run: func(ctx context.Context, r sdk.RunRequest[json.RawMessage]) (sdk.Result, error) {
				o, err := execution.Decode(r.Config, op)
				if err != nil {
					return sdk.Result{}, sdk.Coded("invalid_configuration", err)
				}
				api, err := factory(o)
				if err != nil {
					return sdk.Result{}, sdk.Coded("kubernetes_configuration", err)
				}
				s := &execution.Service{API: api, Namespace: o.Namespace}
				var value any
				switch op {
				case "run":
					value, err = s.Run(ctx, o, r.Stdout)
				case "list":
					value, err = s.List(ctx, o.LabelSelector)
				case "status", "report":
					value, err = s.Status(ctx, o.ExecutionID)
				case "watch":
					value, err = s.Watch(ctx, o.ExecutionID, r.Stdout)
				case "logs":
					err = s.Logs(ctx, o.ExecutionID, o.Follow, o.TailLines, r.Stdout)
				case "cancel":
					value, err = s.Cancel(ctx, o.ExecutionID)
				case "purge":
					err = s.Purge(ctx, o.ExecutionID)
					value = map[string]any{"purged": err == nil}
				}
				outputs := map[string]any{"value": value}
				if st, ok := value.(execution.Status); ok {
					outputs["execution_id"] = st.Record.ExecutionID
					outputs["phase"] = st.Phase
					outputs["report_state"] = st.Record.ReportState
					outputs["termination_confirmed"] = st.TerminationConfirmed
					outputs["report"] = st.Record.Report
					var doc struct {
						Outputs map[string]any `json:"outputs"`
					}
					decoder := json.NewDecoder(bytes.NewReader(st.Record.Report))
					decoder.UseNumber()
					if decoder.Decode(&doc) == nil {
						outputs["outputs"] = doc.Outputs
					}
				}
				return sdk.Result{Outputs: outputs}, sdk.Coded("kubernetes_execution", err)
			},
		}, sdk.WithOutputSchema(sdk.OpenObject(nil)))
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}
func main() {
	p, err := newPlugin(execution.Client)
	if err == nil {
		err = p.Serve(context.Background(), os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
