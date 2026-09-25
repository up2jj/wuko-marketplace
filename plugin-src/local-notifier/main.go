package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

func main() {
	notifier := newNotifier(runtime.GOOS, osCommandRunner{})
	plugin, err := newPlugin(notifier.notify)
	if err == nil {
		err = plugin.Serve(context.Background(), os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
