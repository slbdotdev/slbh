package harness

import (
	"strings"
	"testing"

	"github.com/slbdotdev/slbh/internal/config"
)

func withOutputVariant(t *testing.T, variant string) {
	t.Helper()
	previous, _ := outputVariant.Load().(string)
	outputVariant.Store(variant)
	t.Cleanup(func() { outputVariant.Store(previous) })
}

func TestOutputVariantsCutOverLimitOutput(t *testing.T) {
	long := strings.Repeat("line of output that is long enough\n", 4000)
	short := "fits\n"

	withOutputVariant(t, config.OutputVariantHeadTail)
	headtail := boundedOutput(long)
	if !strings.HasPrefix(headtail, "Warning: truncated output (original token count: ") || strings.Contains(headtail, "read a range") {
		t.Fatalf("headtail notice: %q", headtail[:120])
	}
	if boundedOutput(short) != short {
		t.Fatal("headtail changed output under the limit")
	}

	withOutputVariant(t, config.OutputVariantHint)
	hint := boundedOutput(long)
	if !strings.Contains(hint[:200], "); read a range instead\nTotal output lines: 4000") {
		t.Fatalf("hint notice: %q", hint[:200])
	}
	if strings.Replace(hint, "; read a range instead", "", 1) != headtail {
		t.Fatal("hint differs from headtail by more than the hint")
	}
	if boundedOutput(short) != short {
		t.Fatal("hint changed output under the limit")
	}

	withOutputVariant(t, config.OutputVariantShort)
	got := boundedOutput(long)
	want := "output not shown: 35000 tokens, 4000 lines, over the 20000-token limit; read a range instead"
	if got != want {
		t.Fatalf("short notice:\n got %q\nwant %q", got, want)
	}
	if boundedOutput(short) != short {
		t.Fatal("short changed output under the limit")
	}
}

func TestNormalizeOutputVariant(t *testing.T) {
	if got, err := config.NormalizeOutputVariant(""); err != nil || got != config.OutputVariantHeadTail {
		t.Fatalf("empty: %q %v", got, err)
	}
	if _, err := config.NormalizeOutputVariant("tail"); err == nil {
		t.Fatal("unknown variant accepted")
	}
}
