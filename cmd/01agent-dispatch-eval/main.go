package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type suite struct {
	Name            string   `json:"name"`
	MinimumPassRate float64  `json:"minimum_pass_rate"`
	Cases           []string `json:"cases"`
}

type result struct {
	ID        string   `json:"id"`
	Passed    bool     `json:"passed"`
	Failures  []string `json:"failures,omitempty"`
	LatencyMS int64    `json:"latency_ms"`
}

type report struct {
	Suite       string    `json:"suite"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Cases       int       `json:"cases"`
	Passed      int       `json:"passed"`
	PassRate    float64   `json:"pass_rate"`
	Gate        float64   `json:"gate"`
	GatePassed  bool      `json:"gate_passed"`
	Results     []result  `json:"results"`
}

type testEvent struct {
	Action  string  `json:"Action"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

func main() {
	suitePath := flag.String("suite", "evals/dispatcher.json", "dispatcher evaluation suite JSON")
	reportPath := flag.String("report", "", "optional report JSON path")
	flag.Parse()
	definition, err := loadSuite(*suitePath)
	if err != nil {
		fatal(err)
	}
	started := time.Now().UTC()
	command := exec.Command("go", "test", "-json", "./internal/dispatcher", "-run", "^TestDispatcherEndToEndFaultMatrix$", "-count=1")
	output, runErr := command.Output()
	if exit := new(exec.ExitError); errors.As(runErr, &exit) {
		output = append(output, exit.Stderr...)
	} else if runErr != nil {
		fatal(runErr)
	}
	prefix := "TestDispatcherEndToEndFaultMatrix/"
	observed := map[string]result{}
	logs := map[string][]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || !strings.HasPrefix(event.Test, prefix) {
			continue
		}
		id := strings.TrimPrefix(event.Test, prefix)
		if event.Output != "" {
			logs[id] = append(logs[id], strings.TrimSpace(event.Output))
		}
		if event.Action == "pass" || event.Action == "fail" {
			item := result{ID: id, Passed: event.Action == "pass", LatencyMS: int64(event.Elapsed * 1000)}
			if !item.Passed {
				item.Failures = compact(logs[id])
			}
			observed[id] = item
		}
	}
	if err := scanner.Err(); err != nil {
		fatal(err)
	}
	report := report{Suite: definition.Name, StartedAt: started, CompletedAt: time.Now().UTC(), Cases: len(definition.Cases), Gate: definition.MinimumPassRate}
	for _, id := range definition.Cases {
		item, found := observed[id]
		if !found {
			item = result{ID: id, Failures: []string{"case result was not emitted by dispatcher test harness"}}
		}
		if item.Passed {
			report.Passed++
		}
		report.Results = append(report.Results, item)
	}
	report.PassRate = float64(report.Passed) / float64(report.Cases)
	report.GatePassed = report.PassRate >= report.Gate && runErr == nil
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

func loadSuite(path string) (suite, error) {
	file, err := os.Open(path)
	if err != nil {
		return suite{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var definition suite
	if err := decoder.Decode(&definition); err != nil {
		return suite{}, err
	}
	if definition.Name == "" || len(definition.Cases) != 15 || definition.MinimumPassRate <= 0 || definition.MinimumPassRate > 1 {
		return suite{}, errors.New("dispatcher suite requires a name, exactly 15 cases, and a gate in (0,1]")
	}
	seen := map[string]bool{}
	for _, id := range definition.Cases {
		if strings.TrimSpace(id) == "" || seen[id] {
			return suite{}, fmt.Errorf("invalid or duplicate case %q", id)
		}
		seen[id] = true
	}
	return definition, nil
}

func compact(lines []string) []string {
	var result []string
	for _, line := range lines {
		if line != "" && len(result) < 8 {
			result = append(result, line)
		}
	}
	if len(result) == 0 {
		result = []string{"dispatcher case failed without diagnostic output"}
	}
	return result
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "01agent-dispatch-eval:", err)
	os.Exit(2)
}
