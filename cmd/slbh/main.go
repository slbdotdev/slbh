package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/tui"
)

func main() {
	runtime, err := harness.New(config.Load(), harness.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		os.Exit(1)
	}
	defer runtime.Close()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	// Enable cell-motion mouse reporting so the TUI receives wheel events and
	// can scroll the message viewport while the app is in the alternate screen.
	program := tea.NewProgram(tui.New(runtime))
	go func() {
		<-signals
		_ = runtime.Close()
		program.Quit()
	}()
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		os.Exit(1)
	}
}
