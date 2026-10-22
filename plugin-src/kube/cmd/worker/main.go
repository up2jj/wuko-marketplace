//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
	"github.com/up2jj/wuko-marketplace/plugin-src/kube/supervisor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	c, err := supervisor.LoadControl()
	if err == nil {
		var s execution.Service
		s.Namespace = c.Options.Namespace
		s.API, err = execution.WorkerClient()
		if err == nil {
			err = supervisor.Run(ctx, c, &s)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
