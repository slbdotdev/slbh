// Package orgcli implements the file-backed commands used by the Secretary.
package orgcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/orgstore"
)

// Usage is the help text for the Secretary commands.
const Usage = `slbh secretary commands

  slbh inbox [--json]
  slbh ack <id> [--json]
  slbh request <text> [--json]
  slbh request -f <file> [--json]   ("-" reads stdin)
  slbh requests [--open] [--json]
  slbh help [--json]

Exit status: 0 success, 1 store or I/O error, 2 usage.
`

var commandNames = map[string]struct{}{
	"inbox":    {},
	"ack":      {},
	"request":  {},
	"requests": {},
	"help":     {},
}

// IsCommand reports whether name is a Secretary command.
func IsCommand(name string) bool {
	_, ok := commandNames[name]
	return ok
}

// Run executes a Secretary command. Args starts with the command name.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	asJSON, args, err := takeJSONFlag(args)
	if err != nil {
		return usageError(stderr, asJSON, err.Error())
	}
	if len(args) == 0 {
		return usageError(stderr, asJSON, "missing command")
	}

	command, commandArgs := args[0], args[1:]
	if !IsCommand(command) {
		return usageError(stderr, asJSON, fmt.Sprintf("unknown command %q", command))
	}
	if command == "help" {
		if len(commandArgs) != 0 {
			return usageError(stderr, asJSON, "help takes no arguments")
		}
		if asJSON {
			return writeJSON(stdout, stderr, map[string]string{"usage": Usage})
		}
		_, _ = io.WriteString(stdout, Usage)
		return 0
	}

	store, err := orgstore.Open(config.Load().Home)
	if err != nil {
		return operationalError(stderr, asJSON, err)
	}
	switch command {
	case "inbox":
		return runInbox(store, commandArgs, asJSON, stdout, stderr)
	case "ack":
		return runAck(store, commandArgs, asJSON, stdout, stderr)
	case "request":
		return runRequest(store, commandArgs, asJSON, stdin, stdout, stderr)
	case "requests":
		return runRequests(store, commandArgs, asJSON, stdout, stderr)
	default:
		panic("unreachable Secretary command")
	}
}

func runInbox(store *orgstore.Store, args []string, asJSON bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return usageError(stderr, asJSON, "inbox takes no arguments")
	}
	reports, err := store.Pending()
	if err != nil {
		return operationalError(stderr, asJSON, err)
	}
	if reports == nil {
		reports = []orgstore.Report{}
	}
	if asJSON {
		return writeJSON(stdout, stderr, reports)
	}
	for index, report := range reports {
		if index != 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "report %d\n", report.ID)
		fmt.Fprintf(stdout, "time: %s\n", formatTime(report.Time))
		fmt.Fprintf(stdout, "from: %s\n", report.From)
		fmt.Fprintf(stdout, "text: %s\n", report.Text)
		if report.InvalidatesNone {
			fmt.Fprintln(stdout, "invalidates: none")
		} else {
			fmt.Fprintf(stdout, "invalidates: %s\n", strings.Join(report.Invalidates, ", "))
		}
	}
	return 0
}

func runAck(store *orgstore.Store, args []string, asJSON bool, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return usageError(stderr, asJSON, "ack requires one report id")
	}
	id, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil || id == 0 {
		return usageError(stderr, asJSON, fmt.Sprintf("invalid report id %q", args[0]))
	}
	if err := store.Ack(id); err != nil {
		return operationalError(stderr, asJSON, err)
	}
	if asJSON {
		return writeJSON(stdout, stderr, struct {
			AcknowledgedID uint64 `json:"acknowledged_id"`
		}{AcknowledgedID: id})
	}
	fmt.Fprintf(stdout, "acknowledged report %d\n", id)
	return 0
}

func runRequest(store *orgstore.Store, args []string, asJSON bool, stdin io.Reader, stdout, stderr io.Writer) int {
	var text string
	switch {
	case len(args) == 1 && args[0] != "-f":
		text = args[0]
	case len(args) == 2 && args[0] == "-f":
		var data []byte
		var err error
		if args[1] == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(args[1])
		}
		if err != nil {
			return operationalError(stderr, asJSON, err)
		}
		text = string(data)
	default:
		return usageError(stderr, asJSON, "request requires <text> or -f <file>")
	}
	if strings.TrimSpace(text) == "" {
		return usageError(stderr, asJSON, "request text is empty")
	}
	request, err := store.AppendRequest("secretary", text)
	if err != nil {
		return operationalError(stderr, asJSON, err)
	}
	if asJSON {
		return writeJSON(stdout, stderr, request)
	}
	fmt.Fprintln(stdout, request.ID)
	return 0
}

func runRequests(store *orgstore.Store, args []string, asJSON bool, stdout, stderr io.Writer) int {
	openOnly := false
	for _, arg := range args {
		if arg != "--open" || openOnly {
			return usageError(stderr, asJSON, "requests accepts only one optional --open flag")
		}
		openOnly = true
	}
	requests, err := store.Requests()
	if err != nil {
		return operationalError(stderr, asJSON, err)
	}
	if openOnly {
		filtered := make([]orgstore.Request, 0, len(requests))
		for _, request := range requests {
			if request.Status == orgstore.StatusQueued || request.Status == orgstore.StatusAccepted {
				filtered = append(filtered, request)
			}
		}
		requests = filtered
	}
	if requests == nil {
		requests = []orgstore.Request{}
	}
	if asJSON {
		return writeJSON(stdout, stderr, requests)
	}
	for index, request := range requests {
		if index != 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "request %d\n", request.ID)
		fmt.Fprintf(stdout, "time: %s\n", formatTime(request.Time))
		fmt.Fprintf(stdout, "from: %s\n", request.From)
		fmt.Fprintf(stdout, "text: %s\n", request.Text)
		fmt.Fprintf(stdout, "status: %s\n", request.Status)
		fmt.Fprintln(stdout, "history:")
		for _, change := range request.History {
			fmt.Fprintf(stdout, "  %s %s", formatTime(change.Time), change.Status)
			if change.Note != "" {
				fmt.Fprintf(stdout, ": %s", change.Note)
			}
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

func takeJSONFlag(args []string) (bool, []string, error) {
	filtered := make([]string, 0, len(args))
	asJSON := false
	for _, arg := range args {
		if arg == "--json" {
			if asJSON {
				return true, filtered, fmt.Errorf("--json may be given only once")
			}
			asJSON = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return asJSON, filtered, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, "slbh:", err)
		return 1
	}
	return 0
}

func usageError(stderr io.Writer, asJSON bool, message string) int {
	if asJSON {
		_ = json.NewEncoder(stderr).Encode(map[string]string{"error": message})
	} else {
		fmt.Fprintln(stderr, "slbh:", message)
		fmt.Fprintln(stderr)
		_, _ = io.WriteString(stderr, Usage)
	}
	return 2
}

func operationalError(stderr io.Writer, asJSON bool, err error) int {
	if asJSON {
		_ = json.NewEncoder(stderr).Encode(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintln(stderr, "slbh:", err)
	}
	return 1
}
