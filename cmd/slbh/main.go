package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
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
	program := tea.NewProgram(tui.New(runtime), tea.WithAltScreen(), tea.WithMouseCellMotion())
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
