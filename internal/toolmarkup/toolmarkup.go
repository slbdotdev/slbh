// Package toolmarkup recognizes tool-call markup that a model server returned
// as text instead of as a structured call. The harness answers such a reply as
// a call that did not run; the TUI shows it as a note instead of raw markup.
package toolmarkup

import (
	"regexp"
	"strings"
)

var (
	functionName = regexp.MustCompile(`<function=([^>\s]+)>`)
	jsonName     = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
)

// Split reports whether content ends in a `<tool_call>` region, the markup a
// Qwen-family model writes, and returns the prose before it and the tool
// names the region names so far. A region followed by more prose, or inside a
// code fence, is text about tool calls and is not a trailing region; the
// servers that parse these calls also only take a trailing region. While a
// reply streams, the region may still be unclosed and name no tool yet.
func Split(content string) (prose string, names []string, trailing bool) {
	start := strings.Index(content, "<tool_call>")
	if start < 0 || strings.Count(content[:start], "```")%2 == 1 {
		return content, nil, false
	}
	region := content[start:]
	if last := strings.LastIndex(region, "</tool_call>"); last >= 0 && strings.LastIndex(region, "<tool_call>") < last {
		if strings.TrimSpace(region[last+len("</tool_call>"):]) != "" {
			return content, nil, false
		}
	}
	for _, pattern := range []*regexp.Regexp{functionName, jsonName} {
		for _, match := range pattern.FindAllStringSubmatch(region, -1) {
			names = append(names, match[1])
		}
	}
	return strings.TrimRight(content[:start], " \t\n"), names, true
}
