package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	plugin, err := newPlugin(newService())
	if err == nil {
		err = plugin.Serve(context.Background(), os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
