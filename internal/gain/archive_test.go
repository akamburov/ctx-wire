package gain

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Rotation used to delete the oldest log with its totals, so `ctx-wire gain`
// fell by a whole file every few days. The totals must survive any number of
// rotations while the files on disk stay bounded.
func TestRotationKeepsAllTimeTotals(t *testing.T) {
	path := useTempLog(t)
	oldMax := maxGainLogSize
	maxGainLogSize = 400
	t.Cleanup(func() { maxGainLogSize = oldMax })

	const n = 40
	var wantSaved int64
	wantProg := map[string]int{}
	wantAgent := map[string]int{}
	wantSource := map[string]int{}
	for i := 0; i < n; i++ {
		cmd, ag, src := "git status", "claude", "hook"
		if i%3 == 0 {
			cmd, ag, src = "cat README.md", "codex", "shim"
		}
		raw := 1000 + i
		if err := RecordWithMeta(cmd, "", "filtered", ag, src, raw, 100, 0); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
		wantSaved += int64(raw - 100)
		wantProg[programName(cmd)]++
		wantAgent[ag]++
		wantSource[src]++
	}

	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, maxGainRotations+1)); err == nil {
		t.Fatal("rotation kept more files than maxGainRotations")
	}
	window, err := SummarizeWithOptions(Options{WindowOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if window.Commands >= n {
		t.Fatalf("window holds %d of %d commands; the test needs rotations to delete files", window.Commands, n)
	}

	s, err := Summarize()
	if err != nil {
		t.Fatal(err)
	}
	if s.Commands != n || s.SavedBytes != wantSaved {
		t.Fatalf("all-time = %d commands, %d saved; want %d, %d", s.Commands, s.SavedBytes, n, wantSaved)
	}
	for _, st := range s.ByProgram {
		if st.Count != wantProg[st.Program] {
			t.Errorf("program %s = %d, want %d", st.Program, st.Count, wantProg[st.Program])
		}
	}
	for _, st := range s.ByAgent {
		if st.Commands != wantAgent[st.Agent] {
			t.Errorf("agent %s = %d, want %d", st.Agent, st.Commands, wantAgent[st.Agent])
		}
	}
	for _, st := range s.BySource {
		if st.Commands != wantSource[st.Source] {
			t.Errorf("source %s = %d, want %d", st.Source, st.Commands, wantSource[st.Source])
		}
	}
	if len(s.ByProgram) != len(wantProg) || len(s.ByAgent) != len(wantAgent) || len(s.BySource) != len(wantSource) {
		t.Errorf("breakdown sizes = %d/%d/%d", len(s.ByProgram), len(s.ByAgent), len(s.BySource))
	}

	// A filtered view cannot use the archive (no timestamps, no per-agent
	// programs), so it stays on the retained entries.
	byAgent, err := SummarizeWithOptions(Options{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if byAgent.Commands >= wantAgent["claude"] {
		t.Fatalf("agent view = %d commands, want only the retained ones (< %d)", byAgent.Commands, wantAgent["claude"])
	}

	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archivePath(path)); !os.IsNotExist(err) {
		t.Fatalf("Clear left the archive behind: %v", err)
	}
	if s, _ := Summarize(); s.Commands != 0 {
		t.Fatalf("after Clear = %d commands, want 0", s.Commands)
	}
}

// Every rotation must move the generation, even before any file is deleted:
// the shift alone can make a concurrent reader count a file twice.
func TestRotationBumpsGeneration(t *testing.T) {
	path := useTempLog(t)
	oldMax := maxGainLogSize
	maxGainLogSize = 180
	t.Cleanup(func() { maxGainLogSize = oldMax })

	for i := 0; i < 3; i++ {
		if err := Record("git status", 1000, 100, 0); err != nil {
			t.Fatal(err)
		}
	}
	a, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.Generation == 0 || a.Totals.Count != 0 {
		t.Fatalf("archive after rotations with nothing deleted = %+v, want a generation and no totals", a)
	}
}

// A sandboxed fallback log can rotate on its own; draining it into the primary
// store must carry its archived totals along, or a purged $TMPDIR loses them.
func TestDrainMovesFallbackArchive(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "data", "ctx-wire", "gain.jsonl")
	fallback := filepath.Join(dir, "fallback", "gain.jsonl")
	t.Setenv(envFile, "")
	t.Setenv(envFallbackFile, fallback)
	t.Setenv(envDisable, "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))

	if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(fallback, gainArchive{
		Generation: 2,
		Totals:     archiveStat{Count: 4, RawBytes: 4000, EmittedBytes: 400, SavedBytes: 3600},
		Programs:   map[string]archiveStat{"git": {4, 4000, 400, 3600}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := Record("git primary", 3000, 300, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archivePath(fallback)); !os.IsNotExist(err) {
		t.Fatalf("fallback archive not drained: %v", err)
	}
	a, err := readArchive(primary)
	if err != nil {
		t.Fatal(err)
	}
	if a.Totals.Count != 4 || a.Programs["git"].SavedBytes != 3600 {
		t.Fatalf("primary archive = %+v, want the fallback totals", a)
	}
	s, err := Summarize()
	if err != nil {
		t.Fatal(err)
	}
	if s.Commands != 5 || s.SavedBytes != 3600+2700 {
		t.Fatalf("summary after drain = %d commands, %d saved; want 5, 6300", s.Commands, s.SavedBytes)
	}
}
