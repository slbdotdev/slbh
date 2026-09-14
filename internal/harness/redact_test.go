package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

func TestSecretRedactorReplacesCredentialValuesInToolOutput(t *testing.T) {
	// The shell and Python tools hand a child the parent environment on
	// purpose, so `env` — or a prompt-injected call naming one variable — put a
	// vaulted key into durable JSONL and onto the screen with no boundary
	// anywhere on the path.
	const key = "sk-not-a-real-credential-0001"
	r := newSecretRedactor([]string{"ZAI_API_KEY=" + key, "HOME=/home/slb"})
	got := r.redact("ZAI_API_KEY=" + key + "\nHOME=/home/slb")
	if strings.Contains(got, key) {
		t.Fatalf("the key survived redaction: %q", got)
	}
	if !strings.Contains(got, "[redacted ZAI_API_KEY]") {
		t.Fatalf("redaction did not name the variable: %q", got)
	}
	if !strings.Contains(got, "/home/slb") {
		t.Fatalf("redaction ate text that was not a credential: %q", got)
	}
}

func TestSecretRedactorLeavesShortAndNonCredentialValuesAlone(t *testing.T) {
	// A short or placeholder value would otherwise turn every ordinary
	// occurrence of a common substring into a redaction marker, and a variable
	// that is not a credential has no business being rewritten at all.
	r := newSecretRedactor([]string{
		"ZAI_API_KEY=short",
		"EDITOR=a-long-value-that-is-not-secret",
	})
	const text = "short and a-long-value-that-is-not-secret"
	if got := r.redact(text); got != text {
		t.Fatalf("redacted something it should not have: %q", got)
	}
}

func TestSecretRedactorDoesNotMutateTheCallersMetadata(t *testing.T) {
	// emit hands metadata straight through to the event queue as well as to the
	// log, so rewriting the caller's map in place would change an object the
	// producer may still hold.
	const key = "sk-not-a-real-credential-0002"
	r := newSecretRedactor([]string{"OPENROUTER_API_KEY=" + key})
	original := map[string]any{"name": "quick_bash", "input": "echo " + key, "index": 3}
	cleaned := r.redactMetadata(original)
	if original["input"] != "echo "+key {
		t.Fatalf("the caller's map was mutated: %#v", original)
	}
	if got, _ := cleaned["input"].(string); strings.Contains(got, key) {
		t.Fatalf("the key survived in metadata: %q", got)
	}
	if cleaned["index"] != 3 || cleaned["name"] != "quick_bash" {
		t.Fatalf("non-string and clean values were not carried over: %#v", cleaned)
	}
}

func TestSecretRedactorCoversRawJSONMetadata(t *testing.T) {
	// The request payload is stored as json.RawMessage — a []byte, not a
	// string — so a type switch on string alone skipped the single field most
	// likely to carry a credential: the whole marshalled conversation.
	const key = "sk-not-a-real-credential-0003"
	r := newSecretRedactor([]string{"ZAI_API_KEY=" + key})
	meta := map[string]any{
		"round":   2,
		"payload": json.RawMessage(`{"messages":[{"role":"tool","content":"ZAI_API_KEY=` + key + `"}]}`),
	}
	cleaned := r.redactMetadata(meta)
	raw, ok := cleaned["payload"].(json.RawMessage)
	if !ok {
		t.Fatalf("payload changed kind: %T", cleaned["payload"])
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("the key survived in the request payload: %s", raw)
	}
	if !json.Valid(raw) {
		t.Fatalf("redaction broke the payload's JSON: %s", raw)
	}
	if cleaned["round"] != 2 {
		t.Fatalf("scalar metadata was lost: %#v", cleaned)
	}
}

func TestToolOutputIsRedactedBeforeItReachesHistory(t *testing.T) {
	// Redacting at emit alone left the raw result in history, so the next
	// round marshalled the credential into the request payload — storing it
	// and sending it to the provider. Capture-time redaction is the only one
	// of the two that prevents transmission.
	const key = "sk-not-a-real-credential-0004"
	t.Setenv("ZAI_API_KEY", key)
	r, err := New(
		config.Config{Home: t.TempDir(), SeatModel: "test"},
		Options{Provider: func(string) (provider.Provider, error) { return fakeProvider{}, nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.redactSecrets("leaked ZAI_API_KEY=" + key); strings.Contains(got, key) {
		t.Fatalf("runtime redaction missed the key: %q", got)
	}
}
