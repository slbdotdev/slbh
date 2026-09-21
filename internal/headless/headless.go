// Package headless exposes the slbh runtime over a persistent JSONL protocol.
//
// The protocol is deliberately a thin transport over internal/seam: commands
// are the seam's closed command set, queries return seam snapshots, and every
// runtime event is emitted as an ordered notification. The TUI is another
// consumer of that same seam; it is not part of the runtime lifecycle.
package headless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/slbdotdev/slbh/internal/seam"
)

// Options configures the persistent protocol server.
type Options struct {
	// InitialPrompt is sent to the Seat after the event stream is attached. It
	// is the compatibility bridge for `slbh -p`; subsequent turns arrive as
	// send_prompt protocol commands.
	InitialPrompt string
}

type request struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type decodeResult struct {
	request request
	err     error
}

type writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *writer) write(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(value)
}

// Serve owns the headless protocol loop but not the supplied runtime. It
// remains active until stdin closes, the runtime closes, or ctx is canceled.
// Each input line is one JSON-RPC-shaped request. Runtime events are emitted
// as notifications independently of request/response traffic.
func Serve(ctx context.Context, rt seam.Runtime, in io.Reader, out io.Writer, opts Options) error {
	if rt == nil {
		return errors.New("headless: nil runtime")
	}
	if in == nil {
		return errors.New("headless: nil input")
	}
	if out == nil {
		return errors.New("headless: nil output")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &writer{enc: json.NewEncoder(out)}
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		streamEvents(ctx, rt, w)
	}()

	if opts.InitialPrompt != "" {
		seat, err := seat(rt)
		if err != nil {
			return err
		}
		if _, err := rt.Do(seam.SendPromptCommand{AgentID: seat.ID, Prompt: opts.InitialPrompt}); err != nil {
			return fmt.Errorf("headless: initial prompt: %w", err)
		}
	}

	requests := make(chan decodeResult, 1)
	go decodeRequests(in, requests)
	for {
		var decoded decodeResult
		select {
		case <-ctx.Done():
			<-eventsDone
			return nil
		case <-eventsDone:
			return nil
		case decoded = <-requests:
		}
		if decoded.err != nil {
			if errors.Is(decoded.err, io.EOF) {
				return nil
			}
			return fmt.Errorf("headless: decode request: %w", decoded.err)
		}
		req := decoded.request
		if req.Method == "" {
			if err := writeError(w, req.ID, "invalid_request", "method is required"); err != nil {
				return err
			}
			continue
		}

		result, closeRuntime, err := dispatch(rt, req.Method, req.Params)
		if req.ID != nil {
			if err != nil {
				if writeErr := writeError(w, req.ID, "request_failed", err.Error()); writeErr != nil {
					return writeErr
				}
			} else if writeErr := w.write(response{JSONRPC: "2.0", ID: req.ID, Result: result}); writeErr != nil {
				return writeErr
			}
		}
		if closeRuntime {
			cancel()
			<-eventsDone
			return nil
		}
		select {
		case <-ctx.Done():
			<-eventsDone
			return nil
		default:
		}
	}
}

func decodeRequests(in io.Reader, requests chan<- decodeResult) {
	decoder := json.NewDecoder(in)
	for {
		var req request
		err := decoder.Decode(&req)
		requests <- decodeResult{request: req, err: err}
		if err != nil {
			return
		}
	}
}

func streamEvents(ctx context.Context, rt seam.Runtime, w *writer) {
	var cursor seam.EventCursor
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		batch := rt.PollEvents(seam.EventQuery{After: cursor, Limit: 128, WaitMilliseconds: 250})
		cursor = batch.Cursor
		for _, event := range batch.Events {
			if err := w.write(notification{JSONRPC: "2.0", Method: "event", Params: event}); err != nil {
				return
			}
		}
		if batch.End {
			return
		}
	}
}

func writeError(w *writer, id json.RawMessage, code, message string) error {
	return w.write(response{JSONRPC: "2.0", ID: id, Error: &wireError{Code: code, Message: message}})
}

func seat(rt seam.Runtime) (seam.AgentSnapshot, error) {
	for _, agent := range rt.Agents() {
		if agent.Depth == 0 {
			return agent, nil
		}
	}
	return seam.AgentSnapshot{}, errors.New("headless: runtime has no Seat agent")
}

func dispatch(rt seam.Runtime, method string, params json.RawMessage) (result any, closeRuntime bool, err error) {
	if params == nil {
		params = json.RawMessage(`{}`)
	}
	if seam.IsCommandName(method) {
		command, decodeErr := decodeCommand(method, params)
		if decodeErr != nil {
			return nil, false, decodeErr
		}
		reply, doErr := rt.Do(command)
		return reply, method == seam.CommandClose, doErr
	}

	switch method {
	case "initialize":
		return map[string]any{
			"runtime": rt.ID(),
			"home":    rt.Home(),
			"dir":     rt.Dir(),
			"agents":  rt.Agents(),
		}, false, nil
	case "agents":
		return rt.Agents(), false, nil
	case "jobs":
		return rt.JobSnapshots(), false, nil
	case "quiescent":
		// Optional: a runtime that cannot answer says so rather than guessing.
		if q, ok := rt.(interface {
			Quiescent() bool
			LastEventCursor() seam.EventCursor
		}); ok {
			cursor := q.LastEventCursor()
			return map[string]any{"quiescent": q.Quiescent(), "cursor": cursor}, false, nil
		}
		return nil, false, errors.New("headless: runtime does not report quiescence")
	case "config":
		return rt.Config(), false, nil
	case "runtime":
		return map[string]string{"id": rt.ID(), "home": rt.Home(), "dir": rt.Dir()}, false, nil
	case "transcript_path":
		var query struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(params, &query); err != nil {
			return nil, false, fmt.Errorf("transcript_path params: %w", err)
		}
		path, err := rt.TranscriptPath(query.AgentID)
		return map[string]string{"path": path}, false, err
	case "instruction_source":
		return rt.InstructionSource(), false, nil
	case "skill_source":
		return rt.SkillSource(), false, nil
	case "policy_source":
		return rt.PolicySource(), false, nil
	case "model_catalog":
		return rt.ModelCatalog(), false, nil
	case "model_guidance":
		return rt.ModelGuidance(), false, nil
	case "poll_events":
		var query seam.EventQuery
		if err := json.Unmarshal(params, &query); err != nil {
			return nil, false, fmt.Errorf("poll_events params: %w", err)
		}
		return rt.PollEvents(query), false, nil
	default:
		return nil, false, fmt.Errorf("unknown method %q", method)
	}
}

func decodeCommand(method string, params json.RawMessage) (seam.Command, error) {
	var command seam.Command
	switch method {
	case seam.CommandSendPrompt:
		command = &seam.SendPromptCommand{}
	case seam.CommandSteerAgent:
		command = &seam.SteerAgentCommand{}
	case seam.CommandSetAgentEffort:
		command = &seam.SetAgentEffortCommand{}
	case seam.CommandClear:
		command = &seam.ClearCommand{}
	case seam.CommandCompact:
		command = &seam.CompactCommand{}
	case seam.CommandConfigureModels:
		command = &seam.ConfigureModelsCommand{}
	case seam.CommandConfigureModelSlots:
		command = &seam.ConfigureModelSlotsCommand{}
	case seam.CommandSetModelCatalog:
		command = &seam.SetModelCatalogCommand{}
	case seam.CommandAuthorLocalPolicy:
		command = &seam.AuthorLocalPolicyCommand{}
	case seam.CommandEmitStatus:
		command = &seam.EmitStatusCommand{}
	case seam.CommandClose:
		command = &seam.CloseCommand{}
	default:
		return nil, fmt.Errorf("unknown command %q", method)
	}
	if err := json.Unmarshal(params, command); err != nil {
		return nil, fmt.Errorf("%s params: %w", method, err)
	}
	return commandValue(command), nil
}

// The runtime's closed command set uses value commands. Decode into pointers
// above to keep the JSON decoder simple, then normalize before Do validates
// and dispatches the command.
func commandValue(command seam.Command) seam.Command {
	switch typed := command.(type) {
	case *seam.SendPromptCommand:
		return *typed
	case *seam.SteerAgentCommand:
		return *typed
	case *seam.SetAgentEffortCommand:
		return *typed
	case *seam.ClearCommand:
		return *typed
	case *seam.CompactCommand:
		return *typed
	case *seam.ConfigureModelsCommand:
		return *typed
	case *seam.ConfigureModelSlotsCommand:
		return *typed
	case *seam.SetModelCatalogCommand:
		return *typed
	case *seam.AuthorLocalPolicyCommand:
		return *typed
	case *seam.EmitStatusCommand:
		return *typed
	case *seam.CloseCommand:
		return *typed
	default:
		return command
	}
}
