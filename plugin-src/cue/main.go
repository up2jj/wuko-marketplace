package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == workerArgument {
		if err := serveWorker(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	plugin, err := newPlugin(externalWorker(executable))
	if err == nil {
		err = plugin.Serve(context.Background(), os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
