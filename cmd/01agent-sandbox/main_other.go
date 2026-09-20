//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "01agent-sandbox is only available on Linux")
	os.Exit(126)
}
