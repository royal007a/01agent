package eval

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRuntimeSuitePassesGate(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("..", "..", "evals", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), suite, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.GatePassed || report.Passed != len(suite.Cases) {
		t.Fatalf("report = %#v", report)
	}
}
