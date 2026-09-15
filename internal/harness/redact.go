package harness

import (
	"encoding/json"
	"strings"
)

// secretRedactor replaces the literal value of every credential variable in the
// process environment wherever it appears in text bound for a transcript or the
// screen.
//
// The shell and Python tools hand a child the parent environment deliberately,
// and a background job builds its own from cmd.Environ(), so a key is one `env`
// away from an agent's stdout. That stdout becomes a tool_result event, and
// emit appends the event verbatim to durable JSONL. Redacting at the emit
// boundary keeps the capability — a script that needs a key still receives one
// — while denying the value any path into stored state.
type secretRedactor struct{ repl *strings.Replacer }

// secretEnvSuffixes matches on the variable's name rather than on a shape the
// value might have, because the fleet deploys its whole provider key set to
// every host and a new provider must not need a change here to be covered.
var secretEnvSuffixes = []string{
	"_API_KEY", "_APIKEY", "_TOKEN", "_SECRET", "_SECRET_KEY",
	"_ACCESS_KEY", "_PASSWORD", "_PASSWD", "_CREDENTIALS",
}

// minSecretLen keeps a short or placeholder value from turning every ordinary
// occurrence of a common substring into a redaction marker.
const minSecretLen = 12

func newSecretRedactor(environ []string) *secretRedactor {
	var pairs []string
	seen := make(map[string]bool)
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < minSecretLen || seen[value] {
			continue
		}
		upper := strings.ToUpper(name)
		match := false
		for _, suffix := range secretEnvSuffixes {
			if strings.HasSuffix(upper, suffix) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		seen[value] = true
		pairs = append(pairs, value, "[redacted "+name+"]")
	}
	if len(pairs) == 0 {
		return &secretRedactor{}
	}
	return &secretRedactor{repl: strings.NewReplacer(pairs...)}
}

func (s *secretRedactor) redact(text string) string {
	if s == nil || s.repl == nil || text == "" {
		return text
	}
	return s.repl.Replace(text)
}

// redactMetadata copies before it writes, so an event's metadata map — which
// the producer may still hold a reference to — is never mutated underneath it.
func (s *secretRedactor) redactMetadata(meta map[string]any) map[string]any {
	if s == nil || s.repl == nil || len(meta) == 0 {
		return meta
	}
	var out map[string]any
	for key, value := range meta {
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case json.RawMessage:
			// The request payload is stored as RawMessage — a []byte, not a
			// string — so a type switch on string alone skipped the one field
			// most likely to carry a credential: the whole conversation,
			// marshalled for the wire.
			text = string(typed)
		case []byte:
			text = string(typed)
		default:
			continue
		}
		clean := s.repl.Replace(text)
		if clean == text {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(meta))
			for k, v := range meta {
				out[k] = v
			}
		}
		switch value.(type) {
		case json.RawMessage:
			out[key] = json.RawMessage(clean)
		case []byte:
			out[key] = []byte(clean)
		default:
			out[key] = clean
		}
	}
	if out == nil {
		return meta
	}
	return out
}
