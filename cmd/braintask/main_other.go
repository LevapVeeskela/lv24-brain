//go:build !windows

// Command braintask is the Windows scheduler launcher; other platforms do not
// need a console-suppressing wrapper.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "brain-task is a Windows-only helper; schedule the command directly")
	os.Exit(2)
}
