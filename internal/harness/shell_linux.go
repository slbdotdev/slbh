//go:build linux

package harness

func shellName() string                { return "bash" }
func shellArgs(script string) []string { return []string{"-lc", script} }
