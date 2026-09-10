//go:build !linux

package job

import "runtime"

func isWindows() bool { return runtime.GOOS == "windows" }
