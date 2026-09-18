package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/headless"
	"github.com/slbdotdev/slbh/internal/intern"
	"github.com/slbdotdev/slbh/internal/tui"
)

const usage = `slbh - agent harness

  slbh                          start the TUI
  slbh -p "do the thing"        run one turn headless and exit
  slbh --prompt-file brief.md   same, with the prompt read from a file

Interactive flags:
      --intern          run the read-only Intern watcher

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
	var asJSON, quiet, quietShort, summary, enableIntern bool
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
	fs.BoolVar(&enableIntern, "intern", false, "run the read-only Intern watcher")

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
	internSupplied := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "prompt", "p", "prompt-file":
			supplied = true
		case "intern":
			internSupplied = true
		}
	})

	// No prompt flag at all means the interactive TUI, exactly as before.
	if !supplied {
		runTUI(enableIntern)
		return
	}
	if internSupplied {
		fmt.Fprintln(os.Stderr, "slbh: --intern is available only in interactive mode")
		os.Exit(2)
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
	// Fail fast and say what to do about it. Without a policy every route
	// refuses, and a headless run has no /models menu to author one in, so
	// discovering that as a wrapped error at the first inference round is a
	// worse report than refusing up front. This is a usage problem, not a
	// runtime error, so it takes exit 2.
	if cfg.PolicySource.Kind == config.PolicyNone {
		fmt.Fprintln(os.Stderr, "slbh: no routing policy is in force, so every route refuses.")
		fmt.Fprintf(os.Stderr, "slbh: expected a managed %s, or a local_policy block in %s.\n",
			filepath.Join(cfg.Home, "policy.json"), filepath.Join(cfg.Home, "config.json"))
		fmt.Fprintln(os.Stderr, "slbh: on an unmanaged host, start the TUI, open /models and press p to author one.")
		if cfg.PolicySource.Note != "" {
			fmt.Fprintln(os.Stderr, "slbh:", cfg.PolicySource.Note)
		}
		return 2
	}
	// A degraded resolution — a managed policy present but unreadable or
	// invalid, falling through to the local block the precedence rule allows —
	// is otherwise visible only on the TUI's policy line. A headless run would
	// route on the weaker policy and say nothing, which is the silent
	// substitution ResolvePolicy's own comment promises will not happen.
	if cfg.PolicySource.Note != "" {
		fmt.Fprintln(os.Stderr, "slbh:", cfg.PolicySource.Note)
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

func runTUI(enableIntern bool) {
	runtime, err := harness.New(config.Load(), harness.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		os.Exit(1)
	}
	defer runtime.Close()
	if enableIntern {
		go func() {
			if err := intern.Run(context.Background(), runtime, intern.Options{}); err != nil {
				fmt.Fprintln(os.Stderr, "slbh:", err)
			}
		}()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	// The TUI leaves mouse reporting off so the terminal keeps click and drag
	// for selecting text; /mouse turns it on when wheel scrolling is wanted.
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
