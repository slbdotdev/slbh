package harness

import (
	"fmt"
	"strings"
	"testing"
)

func TestBoundedOutputKeepsTenLinesEachSide(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 4000; i++ {
		fmt.Fprintf(&b, "line %04d of output that is long enough\n", i)
	}
	parts := strings.Split(boundedOutput(b.String()), "\n")
	if parts[0] != "output cut: 40000 tokens, 4000 lines, over the 20000-token limit; the first and last 10 lines are shown; read a range instead" {
		t.Fatalf("notice: %q", parts[0])
	}
	if len(parts) != 1+10+1+10 || parts[1] != "line 0001 of output that is long enough" || parts[10] != "line 0010 of output that is long enough" ||
		parts[11] != "…" || parts[12] != "line 3991 of output that is long enough" || parts[21] != "line 4000 of output that is long enough" {
		t.Fatalf("kept lines: %q", parts)
	}
	if boundedOutput("fits\n") != "fits\n" {
		t.Fatal("output under the limit changed")
	}
}

func TestBoundedOutputCapsLongLines(t *testing.T) {
	got := boundedOutput("start" + strings.Repeat("x", 200000) + "end\n")
	if len(got) > 2400 || !strings.Contains(got, "\nstart") || !strings.HasSuffix(got, "xend") {
		t.Fatalf("one long line was not capped at each end: %d bytes", len(got))
	}
}
