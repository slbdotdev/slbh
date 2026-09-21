package harness

import (
	"fmt"
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

func TestLinesVariantKeepsTenLinesEachSide(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 4000; i++ {
		fmt.Fprintf(&b, "line %04d of output that is long enough\n", i)
	}
	withOutputVariant(t, config.OutputVariantLines)
	got := boundedOutput(b.String())
	parts := strings.Split(got, "\n")
	if !strings.HasPrefix(parts[0], "output cut: ") || !strings.Contains(parts[0], ", 4000 lines, over the 20000-token limit; the first and last 10 lines are shown; read a range instead") {
		t.Fatalf("notice: %q", parts[0])
	}
	if len(parts) != 1+10+1+10 || parts[1] != "line 0001 of output that is long enough" || parts[10] != "line 0010 of output that is long enough" ||
		parts[11] != "…" || parts[12] != "line 3991 of output that is long enough" || parts[21] != "line 4000 of output that is long enough" {
		t.Fatalf("kept lines: %q", parts)
	}
	if boundedOutput("fits\n") != "fits\n" {
		t.Fatal("lines changed output under the limit")
	}
}

func TestLinesVariantCapsLongLines(t *testing.T) {
	withOutputVariant(t, config.OutputVariantLines)
	got := boundedOutput("start" + strings.Repeat("x", 200000) + "end\n")
	if len(got) > 2400 || !strings.Contains(got, "\nstart") || !strings.HasSuffix(got, "xend") {
		t.Fatalf("one long line was not capped at each end: %d bytes, %q … %q", len(got), got[:160], got[len(got)-20:])
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
