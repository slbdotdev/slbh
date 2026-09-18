package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkillFrontMatter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		document        string
		wantName        string
		wantDescription string
		wantError       string
	}{
		{
			name:            "one line",
			document:        "---\nname: reviewer\ndescription: Review a change carefully.\n---\nIgnored body.",
			wantName:        "reviewer",
			wantDescription: "Review a change carefully.",
		},
		{
			name:            "folded",
			document:        "---\nname: reviewer\ndescription: >-\n  Review a change\n  carefully and report.\nother: ignored\n---\n",
			wantName:        "reviewer",
			wantDescription: "Review a change carefully and report.",
		},
		{
			name:      "missing block",
			document:  "name: reviewer\ndescription: no block\n",
			wantError: "missing opening --- block",
		},
		{
			name:      "missing description",
			document:  "---\nname: reviewer\n---\n",
			wantError: "missing description",
		},
		{
			name:            "CRLF",
			document:        "---\r\nname: reviewer\r\ndescription: >-\r\n  Review on Windows\r\n  too.\r\n---\r\nbody\r\n",
			wantName:        "reviewer",
			wantDescription: "Review on Windows too.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSkillFrontMatter([]byte(tc.document))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.wantName || got.Description != tc.wantDescription {
				t.Fatalf("parsed = %#v, want name %q description %q", got, tc.wantName, tc.wantDescription)
			}
		})
	}
}

func writeSkill(t *testing.T, home, layer, directory, document string) string {
	t.Helper()
	dir := filepath.Join(home, SkillsDir, layer, directory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSkillsReadsEachLayerFromSLBHHome(t *testing.T) {
	home := t.TempDir()
	seatPath := writeSkill(t, home, LayerSeat, "review", "---\nname: seat-review\ndescription: Review Seat work.\n---\nbody")
	writeSkill(t, home, LayerManager, "dispatch", "---\nname: dispatch\ndescription: >-\n  Choose a leaf\n  and dispatch it.\n---\n")
	writeSkill(t, home, LayerLeaf, "implement", "---\nname: implement\ndescription: Implement the brief.\n---\n")
	writeSkill(t, home, InstructionIntern, "question", "---\nname: question\ndescription: Ask for evidence.\n---\n")
	t.Setenv("SLBH_HOME", home)

	loaded := Load().Skills
	if loaded.Source.Kind != SkillsManaged || len(loaded.Source.Missing) != 0 || loaded.Source.Note != "" {
		t.Fatalf("source = %#v, want complete managed source", loaded.Source)
	}
	if got := loaded.For(0); len(got) != 1 || got[0].Name != "seat-review" || got[0].Path != seatPath || !filepath.IsAbs(got[0].Path) {
		t.Fatalf("seat skills = %#v", got)
	}
	if got := loaded.For(1); len(got) != 1 || got[0].Description != "Choose a leaf and dispatch it." {
		t.Fatalf("manager skills = %#v", got)
	}
	if got := loaded.For(2); len(got) != 1 || got[0].Name != "implement" {
		t.Fatalf("leaf skills = %#v", got)
	}
	if got := loaded.ForName(InstructionIntern); len(got) != 1 || got[0].Name != "question" {
		t.Fatalf("intern skills = %#v", got)
	}
}

func TestLoadSkillsDegradesAndRecordsEveryFailure(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, SkillsDir, LayerSeat, "missing-file"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, home, LayerSeat, "invalid", "---\nname: invalid\n---\n")
	t.Setenv("SLBH_HOME", home)

	loaded := Load().Skills
	if got := loaded.For(0); len(got) != 0 {
		t.Fatalf("invalid skills were loaded: %#v", got)
	}
	for _, want := range []string{
		"missing-file/SKILL.md is missing",
		"invalid/SKILL.md has unparseable front matter: missing description",
		"skill layer directory " + filepath.Join(home, SkillsDir, LayerManager) + " is missing",
	} {
		if !strings.Contains(loaded.Source.Note, want) {
			t.Fatalf("source note = %q, want %q", loaded.Source.Note, want)
		}
	}
	if strings.Join(loaded.Source.Missing, ",") != LayerManager+","+LayerLeaf+","+InstructionIntern {
		t.Fatalf("missing layers = %v", loaded.Source.Missing)
	}
}

func TestLoadSkillsMissingRootIsVisible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLBH_HOME", home)
	loaded := Load().Skills
	if loaded.Source.Kind != SkillsNone {
		t.Fatalf("source kind = %q, want %q", loaded.Source.Kind, SkillsNone)
	}
	if !strings.Contains(loaded.Source.Note, "skills directory") || !strings.Contains(loaded.Source.Note, "is missing") {
		t.Fatalf("missing directory was silent: %#v", loaded.Source)
	}
	if !strings.Contains(loaded.Source.Describe(), "without slbh skills") {
		t.Fatalf("description = %q", loaded.Source.Describe())
	}
}
