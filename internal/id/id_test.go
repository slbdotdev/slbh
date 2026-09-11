package id

import (
	"strings"
	"testing"
)

func TestNewShortUsesEightRandomCharacters(t *testing.T) {
	got := NewShort("agent")
	if !strings.HasPrefix(got, "agent-") {
		t.Fatalf("NewShort() = %q, want agent prefix", got)
	}
	if got := len(strings.TrimPrefix(got, "agent-")); got != 8 {
		t.Fatalf("NewShort() random suffix length = %d, want 8", got)
	}
}
