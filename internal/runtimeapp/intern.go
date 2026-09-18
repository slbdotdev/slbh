// Package runtimeapp contains lifecycle pieces shared by slbh front ends.
package runtimeapp

import (
	"context"

	"github.com/slbdotdev/slbh/internal/intern"
	"github.com/slbdotdev/slbh/internal/seam"
)

// StartIntern starts the runtime's read-only Intern watcher. The watcher is a
// runtime concern, not a TUI concern, so both the TUI and headless front ends
// use this helper. Startup failures are emitted as runtime status events; an
// Intern inference failure is already reported by intern.Run itself.
func StartIntern(ctx context.Context, runtime seam.Runtime) (stop func()) {
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := intern.Run(watchCtx, runtime, intern.Options{}); err != nil && watchCtx.Err() == nil {
			_, _ = runtime.Do(seam.EmitStatusCommand{Kind: "intern_error", Text: err.Error()})
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
