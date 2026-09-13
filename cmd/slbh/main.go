package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/headless"
	"github.com/slbdotdev/slbh/internal/tui"
)

const usage = `slbh - agent harness

  slbh                          start the TUI
  slbh -p "do the thing"        run one turn headless and exit
  slbh --prompt-file brief.md   same, with the prompt read from a file

Headless flags:
  -p, --prompt STRING    prompt text; running with one implies headless
      --prompt-file PATH read the prompt from a file ("-" for stdin)
      --workdir PATH     directory the agent works in (default: cwd)
      --model SLUG       model for the turn (default: configured seat model)
      --effort LEVEL     thinking effort for the turn
      --timeout DURATION wall cap, e.g. 15m; zero means none
      --json             one JSON object per event on stdout
  -q, --quiet            no event stream; only the final summary
      --summary          print the run summary as JSON on exit

Exit status: 0 turn completed, 1 error (the seat failed or could not start), 2 usage,
124 wall cap reached.
`

func main() {
	fs := flag.NewFlagSet("slbh", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var prompt, promptShort, promptFile, workdir, model, effort string
	var timeout time.Duration
	var asJSON, quiet, quietShort, summary bool
	fs.StringVar(&prompt, "prompt", "", "prompt text")
	fs.StringVar(&promptShort, "p", "", "prompt text (short)")
	fs.StringVar(&promptFile, "prompt-file", "", "read the prompt from a file")
	fs.StringVar(&workdir, "workdir", "", "directory the agent works in")
	fs.StringVar(&model, "model", "", "model for the turn")
	fs.StringVar(&effort, "effort", "", "thinking effort for the turn")
	fs.DurationVar(&timeout, "timeout", 0, "wall cap")
	fs.BoolVar(&asJSON, "json", false, "one JSON object per event")
	fs.BoolVar(&quiet, "quiet", false, "no event stream")
	fs.BoolVar(&quietShort, "q", false, "no event stream (short)")
	fs.BoolVar(&summary, "summary", false, "print the run summary as JSON")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if promptShort != "" {
		prompt = promptShort
	}
	quiet = quiet || quietShort

	// Whether a prompt flag was SUPPLIED, not whether it is non-empty: `slbh -p ""` is a
	// caller who meant to run headless and got the prompt wrong, and silently opening the
	// TUI instead would strand a script on a terminal it does not have.
	supplied := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "prompt", "p", "prompt-file":
			supplied = true
		}
	})

	// No prompt flag at all means the interactive TUI, exactly as before.
	if !supplied {
		runTUI()
		return
	}

	if promptFile != "" {
		if prompt != "" {
			fmt.Fprintln(os.Stderr, "slbh: --prompt and --prompt-file are mutually exclusive")
			os.Exit(2)
		}
		var b []byte
		var err error
		if promptFile == "-" {
			b, err = readAll(os.Stdin)
		} else {
			b, err = os.ReadFile(promptFile)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "slbh:", err)
			os.Exit(2)
		}
		prompt = string(b)
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(os.Stderr, "slbh: the prompt is empty")
		os.Exit(2)
	}
	os.Exit(runHeadless(prompt, workdir, model, effort, timeout, asJSON, quiet, summary))
}

func runHeadless(prompt, workdir, model, effort string, timeout time.Duration, asJSON, quiet, summary bool) int {
	// The runtime takes its working directory from the process, so chdir before New.
	if workdir != "" {
		if err := os.Chdir(workdir); err != nil {
			fmt.Fprintln(os.Stderr, "slbh:", err)
			return 2
		}
	}
	cfg := config.Load()
	if model != "" {
		// An explicit --model is an approval for this turn: a caller that named the model
		// has already made the cost decision the approved list exists to protect.
		cfg.SeatModel = model
		if !cfg.ModelApproved(model) {
			cfg.ApprovedModels = append(cfg.ApprovedModels, model)
		}
	}
	if effort != "" {
		cfg.SeatEffort = effort
	}

	rt, err := harness.New(cfg, harness.Options{Config: cfg})
	if err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		return 1
	}
	defer rt.Close()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		_ = rt.Close()
	}()

	res, err := headless.Run(rt, headless.Options{
		Prompt:  prompt,
		Timeout: timeout,
		JSON:    asJSON,
		Quiet:   quiet,
		Out:     os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		return 1
	}
	if summary || quiet {
		printSummary(res)
	}
	switch res.StopReason {
	case "wall_cap":
		return 124
	case "error":
		return 1
	}
	return 0
}

func runTUI() {
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

func readAll(f *os.File) ([]byte, error) {
	return io.ReadAll(f)
}

func printSummary(res headless.Result) {
	b, err := json.Marshal(res)
	if err != nil {
		return
	}
	fmt.Println(string(b))
}
