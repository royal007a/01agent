package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/royal007a/01agent/internal/runstore"
)

func main() {
	tracePath := flag.String("trace", "", "path to a .trace.jsonl file")
	flag.Parse()
	if *tracePath == "" {
		fmt.Fprintln(os.Stderr, "01agent-replay: --trace is required")
		os.Exit(2)
	}
	trace, err := runstore.LoadTrace(*tracePath)
	if err != nil {
		fatal(err)
	}
	summary, err := runstore.Replay(trace)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "01agent-replay:", err)
	os.Exit(1)
}
