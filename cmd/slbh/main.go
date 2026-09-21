package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/harness"
	"github.com/slbdotdev/slbh/internal/headless"
	"github.com/slbdotdev/slbh/internal/runtimeapp"
	"github.com/slbdotdev/slbh/internal/tui"
)

const usage = `slbh - agent harness

  slbh                          start the TUI
  slbh --headless                run the persistent JSONL runtime protocol
  slbh -p "do the thing"        start headless with an initial Seat prompt
  slbh --prompt-file brief.md   same, with the initial prompt read from a file

Runtime flags:
      --headless          run the persistent JSONL runtime protocol
      --intern            start the read-only Intern watcher in any frontend
  -p, --prompt STRING      initial prompt for the Seat; implies --headless
      --prompt-file PATH   initial prompt file; implies --headless
      --workdir PATH       directory the agent works in (default: cwd)
      --model SLUG         model for the runtime Seat
      --effort LEVEL       reasoning effort for the runtime Seat
      --tool-shape SHAPE   primary tools for native agents: lean (default),
                           mid, full, anthropic or codex; overrides
                           SLBH_TOOL_SHAPE and config.json's tool_shape

Exit status: 0 clean shutdown, 1 runtime or protocol error, 2 usage.
`

func main() {
	fs := flag.NewFlagSet("slbh", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var prompt, promptShort, promptFile, workdir, model, effort, toolShape string
	var headlessMode, enableIntern bool
	fs.StringVar(&prompt, "prompt", "", "prompt text")
	fs.StringVar(&promptShort, "p", "", "prompt text (short)")
	fs.StringVar(&promptFile, "prompt-file", "", "read the prompt from a file")
	fs.StringVar(&workdir, "workdir", "", "directory the agent works in")
	fs.StringVar(&model, "model", "", "model for the runtime Seat")
	fs.StringVar(&effort, "effort", "", "reasoning effort for the runtime Seat")
	fs.StringVar(&toolShape, "tool-shape", "", "primary tool shape: lean (default), mid, full, anthropic or codex")
	fs.BoolVar(&headlessMode, "headless", false, "run the persistent JSONL runtime protocol")
	fs.BoolVar(&enableIntern, "intern", false, "run the read-only Intern watcher")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if promptShort != "" {
		prompt = promptShort
	}
	// The flag is the highest-precedence source and travels as the
	// environment variable, so the TUI and headless read it the same way. The
	// effective shape from any source is checked here, before a runtime
	// exists: a mistyped shape must stop the program, not fall back.
	if toolShape != "" {
		os.Setenv("SLBH_TOOL_SHAPE", toolShape)
	}
	if _, err := config.NormalizeToolShape(config.Load().ToolShape); err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
		os.Exit(2)
	}
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
	if headlessMode {
		supplied = true
	}

	// No prompt flag at all means the interactive TUI, exactly as before.
	if !supplied {
		runTUI(enableIntern)
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
	if supplied && !headlessMode && strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(os.Stderr, "slbh: the prompt is empty")
		os.Exit(2)
	}
	os.Exit(runHeadless(prompt, workdir, model, effort, enableIntern))
}

func runHeadless(prompt, workdir, model, effort string, enableIntern bool) int {
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
	// refuses, and a headless runtime has no /models menu to author one in, so
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if enableIntern {
		stopIntern := runtimeapp.StartIntern(ctx, rt)
		defer stopIntern()
	}
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()
	if err := headless.Serve(ctx, rt, os.Stdin, os.Stdout, headless.Options{InitialPrompt: prompt}); err != nil {
		fmt.Fprintln(os.Stderr, "slbh:", err)
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
		stopIntern := runtimeapp.StartIntern(context.Background(), runtime)
		defer stopIntern()
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
