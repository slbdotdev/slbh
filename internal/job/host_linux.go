//go:build linux

package job

func AvailableInterpreters() []string { return []string{"bash", "python"} }
func BashDescription() string         { return "Run a Bash script with GNU Bash on Linux." }
