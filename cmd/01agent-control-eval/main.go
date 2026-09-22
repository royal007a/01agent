package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/royal007a/01agent/internal/workitem"
)

func main() {
	suitePath := flag.String("suite", "evals/control-plane.json", "control-plane evaluation suite JSON")
	reportPath := flag.String("report", "", "optional report JSON path")
	flag.Parse()
	suite, err := workitem.LoadEvaluationSuite(*suitePath)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := workitem.RunEvaluation(ctx, suite)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(encoded))
	if *reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(*reportPath), 0o700); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*reportPath, append(encoded, '\n'), 0o600); err != nil {
			fatal(err)
		}
	}
	if !report.GatePassed {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "01agent-control-eval:", err)
	os.Exit(2)
}
