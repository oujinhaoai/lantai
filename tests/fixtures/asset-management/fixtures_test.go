package assetmanagement

import (
	"os/exec"
	"testing"
)

// Keep fixture contract and corruption checks in the ordinary go test suite.
// Media generation tests explicitly skip when FFmpeg is absent; no test installs
// a tool, reads an existing collection, or runs Blender/ComfyUI implicitly.
func TestSyntheticFixtureGenerator(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for synthetic fixture contract tests")
	}
	command := exec.Command(python, "-m", "unittest", "discover", "-s", ".", "-p", "test_generate.py", "-v")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic fixture tests: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
