package harness

import (
	"strings"
	"testing"
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
