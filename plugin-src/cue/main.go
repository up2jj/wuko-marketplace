package main

import (
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
	if err := serve(os.Stdin, os.Stdout, externalWorker(executable)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
