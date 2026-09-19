package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	agenteval "github.com/royal007a/01agent/internal/eval"
)

func main() {
	suitePath := flag.String("suite", "evals/runtime.json", "evaluation suite JSON")
	artifacts := flag.String("artifacts", "artifacts/eval", "trace and result directory")
	reportPath := flag.String("report", "", "optional report JSON path")
	flag.Parse()

	suite, err := agenteval.LoadSuite(*suitePath)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*artifacts, 0o700); err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := agenteval.Run(ctx, suite, *artifacts)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(encoded))
	if *reportPath != "" {
		if err := os.WriteFile(*reportPath, append(encoded, '\n'), 0o600); err != nil {
			fatal(err)
		}
	}
	if !report.GatePassed {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "01agent-eval:", err)
	os.Exit(2)
}
