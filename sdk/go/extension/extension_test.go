package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func run(t *testing.T, job any) *Run {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"in", "out"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(job)
	p := filepath.Join(dir, "job.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open([]string{"entry", p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCommandResultListsExactOutputs(t *testing.T) {
	r := run(t, CommandJob{Contract: CommandInputContract, OperationID: "01K00000000000000000000009", Command: "org.example.tools.inspect", Args: []string{}, Inputs: []CommandInput{}})
	j, err := r.Command()
	if err != nil || j.Command != "org.example.tools.inspect" {
		t.Fatal(j, err)
	}
	if _, err = r.Processor(); err == nil {
		t.Fatal("processor view accepted a command job")
	}
	for _, bad := range []string{"", "../x", "/abs", "result.json", `a\b`, "a/../b"} {
		if _, err = r.Output(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	p, err := r.Output("report/summary.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = r.FinishCommand(CommandResult{OperationID: j.OperationID, Command: j.Command, Status: "completed", Producer: j.Producer}); err != nil {
		t.Fatal(err)
	}
	var res CommandResult
	b, _ := os.ReadFile(filepath.Join(r.dir, "out", "result.json"))
	if err = json.Unmarshal(b, &res); err != nil || len(res.Files) != 1 || res.Files[0].Path != "report/summary.txt" || res.Files[0].Size != 2 || res.Contract != CommandResultContract {
		t.Fatal(res, err)
	}
	if _, err = Open([]string{"entry", "relative/job.json"}); err == nil {
		t.Fatal("relative job accepted")
	}
	if _, err = r.Input("../job.json", 10); err == nil {
		t.Fatal("input traversal accepted")
	}
}
