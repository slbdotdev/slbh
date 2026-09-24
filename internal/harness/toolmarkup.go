package harness

import (
	"fmt"
	"strings"

	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/toolmarkup"
)

// A model server that cannot parse a tool call returns the model's markup as
// ordinary text, with no structured call. ninfer-serve does this for a whole
// response when any call names a tool the request did not declare: a Qwen
// agent on the lean shape calling read_file, which lean does not offer, had
// its bash call in the same response dropped with it. Taken as a final answer,
// that markup ended the turn silently, and a subagent delivered it to its
// parent as its result. It is answered instead as a failed tool call is: the
// model is told what was not run and why, and the turn goes on.
//
// maxToolMarkupRetries bounds how many such replies in a row are answered
// before the turn fails with the reason, so a model that cannot stop does not
// spend the round limit.
const maxToolMarkupRetries = 3

// leakedToolCalls reports whether content ends in tool-call markup that names
// at least one tool, and returns the names (toolmarkup.Split).
func leakedToolCalls(content string) ([]string, bool) {
	_, names, trailing := toolmarkup.Split(content)
	if !trailing || len(names) == 0 {
		return nil, false
	}
	return names, true
}

// toolMarkupNote tells the model why its markup was not run.
func toolMarkupNote(names []string, tools []provider.Tool) string {
	declared := make(map[string]bool, len(tools))
	available := make([]string, 0, len(tools))
	for _, tool := range tools {
		declared[tool.Name] = true
		available = append(available, tool.Name)
	}
	var undeclared []string
	for _, name := range names {
		if !declared[name] && !contains(undeclared, name) {
			undeclared = append(undeclared, name)
		}
	}
	note := "[harness] Your last reply ended in tool-call markup that was not executed: the model server returned it as text, so no tool ran."
	if len(undeclared) > 0 {
		note += fmt.Sprintf(" %s is not one of your tools, and a call to an undeclared tool makes the server drop every call in that reply.", strings.Join(undeclared, ", "))
	} else {
		note += " The markup could not be parsed as a call."
	}
	return note + " Your tools are: " + strings.Join(available, ", ") + ". Call one of them, or answer without a tool call."
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
