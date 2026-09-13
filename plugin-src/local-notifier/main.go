package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	notifier := newNotifier(runtime.GOOS, osCommandRunner{})
	if err := serve(os.Stdin, os.Stdout, notifier.notify); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
