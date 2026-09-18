package secretarywake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretaryWakeDoesNotDependOnHarness(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "list", "-deps", "./internal/secretarywake")
	command.Dir = root
	command.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list dependencies: %v\n%s", err, output)
	}
	wanted := "github.com/slbdotdev/slbh/internal/harness"
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == wanted {
			t.Fatalf("internal/secretarywake depends on %s", wanted)
		}
	}
}
