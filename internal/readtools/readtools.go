// Package readtools provides the harness's file-reading tools without any
// execution or write capability.
package readtools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/slbdotdev/slbh/internal/provider"
)

// Definitions returns the read-only tool definitions in their stable provider
// prefix order.
func Definitions() []provider.Tool {
	stringArg := func(name string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{name: map[string]any{"type": "string"}}, "required": []string{name}}
	}
	return []provider.Tool{
		{Name: "glob", Description: "List files and directories matching a glob pattern. `**` matches any number of directory levels, so `**/*.py` finds every Python file in the tree and `**/*` lists the whole tree. Directories come back with a trailing separator. Says so explicitly when nothing matches.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "Glob pattern, relative to the working directory unless it is absolute. Supports *, ?, [...] within one path segment and ** across segments."}}, "required": []string{"pattern"}}},
		{Name: "grep", Description: "Search text using a regular expression.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}, "required": []string{"pattern"}}},
		{Name: "read_file", Description: "Read a whole file up to 100k bytes.", Parameters: stringArg("path")},
		{Name: "read_bytes", Description: "Read an inclusive byte range from a file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}}, "required": []string{"path", "start", "end"}}},
		{Name: "read_lines", Description: "Read an inclusive line range from a file.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}}, "required": []string{"path", "start", "end"}}},
	}
}

// Execute runs one read-only tool relative to workingDir.
func Execute(workingDir, name, raw string) (string, error) {
	values, err := parseArgs(raw)
	if err != nil {
		return "", err
	}
	switch name {
	case "glob":
		return glob(workingDir, value(values, "pattern"))
	case "grep":
		return grep(workingDir, value(values, "pattern"), valueDefault(values, "path", "."))
	case "read_file":
		return readFile(workingDir, value(values, "path"))
	case "read_bytes":
		return readBytes(workingDir, value(values, "path"), intValue(values, "start"), intValue(values, "end"))
	case "read_lines":
		return readLines(workingDir, value(values, "path"), intValue(values, "start"), intValue(values, "end"))
	default:
		return "", fmt.Errorf("unknown read tool %q", name)
	}
}

func parseArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("tool arguments must be JSON: %w", err)
	}
	return values, nil
}

func value(values map[string]any, key string) string { return valueDefault(values, key, "") }

func valueDefault(values map[string]any, key, fallback string) string {
	if v, ok := values[key].(string); ok {
		return v
	}
	return fallback
}

func intValue(values map[string]any, key string) int {
	if v, ok := values[key].(float64); ok {
		return int(v)
	}
	if v, ok := values[key].(int); ok {
		return v
	}
	return 0
}

func resolvePath(base, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	clean, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return clean, nil
}

const maxGlobMatches = 400

var globSkipDirs = map[string]bool{
	".git": true, "__pycache__": true, ".pytest_cache": true,
	"node_modules": true, ".venv": true, ".mypy_cache": true, ".tox": true,
}

func glob(base, pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	original := pattern
	absolute := filepath.IsAbs(pattern)
	pattern, err := resolvePath(base, pattern)
	if err != nil {
		return "", err
	}
	var matches []string
	if strings.Contains(pattern, "**") {
		matches, err = walkGlob(pattern)
	} else {
		matches, err = filepath.Glob(pattern)
	}
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	truncated := 0
	if len(matches) > maxGlobMatches {
		truncated = len(matches) - maxGlobMatches
		matches = matches[:maxGlobMatches]
	}
	lines := make([]string, 0, len(matches))
	for _, match := range matches {
		display := match
		if !absolute {
			if rel, relErr := filepath.Rel(base, match); relErr == nil {
				display = rel
			}
		}
		if info, statErr := os.Stat(match); statErr == nil && info.IsDir() {
			display += string(filepath.Separator)
		}
		lines = append(lines, display)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("no files match %q (searched from %s). `**` matches any number of directories; try a broader pattern such as **/* to list the tree.", original, globSearchRoot(pattern)), nil
	}
	out := strings.Join(lines, "\n")
	if truncated > 0 {
		out += fmt.Sprintf("\n[%d more matches not shown; narrow the pattern]", truncated)
	}
	return out, nil
}

func globSearchRoot(pattern string) string {
	segments := strings.Split(filepath.ToSlash(pattern), "/")
	root := ""
	for _, segment := range segments {
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		root += segment + "/"
	}
	if root == "" {
		return string(filepath.Separator)
	}
	return filepath.FromSlash(strings.TrimSuffix(root, "/"))
}

func walkGlob(pattern string) ([]string, error) {
	root := globSearchRoot(pattern)
	if root == "" {
		root = string(filepath.Separator)
	}
	patternSegments := strings.Split(filepath.ToSlash(pattern), "/")
	var matches []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if info.IsDir() && path != root && globSkipDirs[info.Name()] {
			return filepath.SkipDir
		}
		if matchSegments(patternSegments, strings.Split(filepath.ToSlash(path), "/")) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil && len(matches) == 0 {
		return nil, err
	}
	return matches, nil
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := filepath.Match(pattern[0], name[0])
		if err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

func grep(base, pattern, path string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	absolute := filepath.IsAbs(path)
	walkPath, err := resolvePath(base, path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	err = filepath.Walk(walkPath, func(file string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > 2*1024*1024 {
			return nil
		}
		f, err := os.Open(file)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		line := 0
		for scanner.Scan() {
			line++
			if re.MatchString(scanner.Text()) {
				display := file
				if !absolute {
					display, _ = filepath.Rel(base, file)
				}
				fmt.Fprintf(&out, "%s:%d:%s\n", display, line, scanner.Text())
			}
		}
		return scanner.Err()
	})
	return out.String(), err
}

func readFile(base, path string) (string, error) {
	file, err := resolvePath(base, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if info.Size() > 100*1024 {
		data, _ := os.ReadFile(file)
		return "", fmt.Errorf("file is %d bytes, %d lines, type %s; use read_lines or read_bytes instead", info.Size(), bytes.Count(data, []byte{'\n'})+1, detectType(file))
	}
	b, err := os.ReadFile(file)
	return string(b), err
}

func readBytes(base, path string, start, end int) (string, error) {
	if start < 0 || end < start || end-start+1 > 100*1024 {
		return "", fmt.Errorf("byte range must be zero-based, inclusive, and at most 100k bytes")
	}
	file, err := resolvePath(base, path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if start >= len(data) {
		return "", fmt.Errorf("byte start %d is past file size %d", start, len(data))
	}
	if end >= len(data) {
		end = len(data) - 1
	}
	return string(data[start : end+1]), nil
}

func detectType(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer file.Close()
	var header [512]byte
	n, _ := file.Read(header[:])
	return http.DetectContentType(header[:n])
}

func readLines(base, path string, start, end int) (string, error) {
	if start < 1 || end < start {
		return "", fmt.Errorf("line range must be one-based and inclusive")
	}
	file, err := resolvePath(base, path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	var out strings.Builder
	line := 0
	for s.Scan() {
		line++
		if line >= start && line <= end {
			fmt.Fprintf(&out, "%d:%s\n", line, s.Text())
		}
		if line > end {
			break
		}
	}
	return out.String(), s.Err()
}
