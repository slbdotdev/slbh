//go:build !linux && !windows

package job

func AvailableInterpreters() []string { return []string{"bash", "python"} }
func BashDescription() string         { return "Run a Bash script with the host Bash interpreter." }
