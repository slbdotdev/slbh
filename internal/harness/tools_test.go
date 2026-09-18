package harness

import (
	"strings"
	"testing"
)

func TestPythonToolDescriptionsNameScientificPackages(t *testing.T) {
	want := strings.Split(scientificPythonPackageNames, ", ")
	for _, tool := range ToolDefinitions() {
		if tool.Name != "quick_py" && tool.Name != "long_py" {
			continue
		}
		for _, packageName := range want {
			if !strings.Contains(tool.Description, packageName) {
				t.Errorf("%s description does not name %s: %q", tool.Name, packageName, tool.Description)
			}
		}
	}
}
