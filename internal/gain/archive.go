package gain

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Rotation deletes the oldest gain log once the family is full, which made the
// summary a sliding window: its totals fell by a whole file at every rotation.
// Before that file is deleted, its totals are folded into a small archive next
// to the log, and summaries add the archive back, so all-time totals only grow
// while disk use stays bounded. History, the period views, and opportunities
// still read the retained entries only.

// archiveStat is one archived bucket (a program, agent, or source).
type archiveStat struct {
	Count        int   `json:"count"`
	RawBytes     int64 `json:"raw_bytes"`
	EmittedBytes int64 `json:"emitted_bytes"`
	SavedBytes   int64 `json:"saved_bytes"`
}

func (s *archiveStat) add(o archiveStat) {
	s.Count += o.Count
	s.RawBytes += o.RawBytes
	s.EmittedBytes += o.EmittedBytes
	s.SavedBytes += o.SavedBytes
}

// gainArchive holds the totals of every rotated-out log of one log family.
type gainArchive struct {
	// Generation increases on every rotation, so a reader can tell that the
	// family shifted while it was being read.
	Generation int                    `json:"generation"`
	Totals     archiveStat            `json:"totals"`
	Programs   map[string]archiveStat `json:"programs,omitempty"`
	Agents     map[string]archiveStat `json:"agents,omitempty"`
	Sources    map[string]archiveStat `json:"sources,omitempty"`
}

// archivePath returns the archive file for the log family rooted at logPath.
func archivePath(logPath string) string {
	return strings.TrimSuffix(logPath, ".jsonl") + "-archive.json"
}

// readArchive loads the archive of logPath's family. A missing archive is
// empty. A corrupt one is also treated as empty, so the next rotation starts it
// over instead of failing forever; only an I/O error is returned.
func readArchive(logPath string) (gainArchive, error) {
	data, err := os.ReadFile(archivePath(logPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gainArchive{}, nil
		}
		return gainArchive{}, err
	}
	var a gainArchive
	if err := json.Unmarshal(data, &a); err != nil {
		return gainArchive{}, nil
	}
	return a, nil
}

// writeArchive replaces the archive atomically, so a concurrent reader sees the
// old or the new totals, never a torn file.
func writeArchive(logPath string, a gainArchive) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	path := archivePath(logPath)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gain-archive-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// merge adds o's totals into a. The generation is left to the caller.
func (a *gainArchive) merge(o gainArchive) {
	a.Totals.add(o.Totals)
	a.Programs = mergeArchiveStats(a.Programs, o.Programs)
	a.Agents = mergeArchiveStats(a.Agents, o.Agents)
	a.Sources = mergeArchiveStats(a.Sources, o.Sources)
}

func mergeArchiveStats(dst, src map[string]archiveStat) map[string]archiveStat {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]archiveStat, len(src))
	}
	for name, st := range src {
		cur := dst[name]
		cur.add(st)
		dst[name] = cur
	}
	return dst
}

// retiredTotals aggregates one log file the way a summary would, without
// opportunities. A missing file yields an empty archive.
func retiredTotals(path string) (gainArchive, error) {
	sum := &Summary{}
	byProg := map[string]*CommandStat{}
	byAgent := map[string]*AgentStat{}
	bySource := map[string]*SourceStat{}
	if err := summarizePath(path, Options{}, sum, byProg, nil, byAgent, bySource); err != nil {
		return gainArchive{}, err
	}
	a := gainArchive{
		Totals:   archiveStat{sum.Commands, sum.RawBytes, sum.EmittedBytes, sum.SavedBytes},
		Programs: make(map[string]archiveStat, len(byProg)),
		Agents:   make(map[string]archiveStat, len(byAgent)),
		Sources:  make(map[string]archiveStat, len(bySource)),
	}
	for name, st := range byProg {
		a.Programs[name] = archiveStat{st.Count, st.RawBytes, st.EmittedBytes, st.SavedBytes}
	}
	for name, st := range byAgent {
		a.Agents[name] = archiveStat{st.Commands, st.RawBytes, st.EmittedBytes, st.SavedBytes}
	}
	for name, st := range bySource {
		a.Sources[name] = archiveStat{st.Commands, st.RawBytes, st.EmittedBytes, st.SavedBytes}
	}
	return a, nil
}

// recordRotation bumps the family's generation and, when retired is non-nil,
// adds the deleted file's totals. Called with the family's lock held.
func recordRotation(logPath string, retired *gainArchive) error {
	a, err := readArchive(logPath)
	if err != nil {
		return err
	}
	a.Generation++
	if retired != nil {
		a.merge(*retired)
	}
	return writeArchive(logPath, a)
}

// moveArchive folds the fallback family's archive into the primary one and
// removes it, so a drained sandbox log keeps its rotated-out totals. Called
// with both families' locks held.
func moveArchive(fallback, primary string) error {
	src, err := readArchive(fallback)
	if err != nil {
		return err
	}
	dst, err := readArchive(primary)
	if err != nil {
		return err
	}
	dst.Generation++
	dst.merge(src)
	if err := writeArchive(primary, dst); err != nil {
		return err
	}
	return os.Remove(archivePath(fallback))
}

// addTo adds the archived totals into a summary under construction.
func (a gainArchive) addTo(sum *Summary, byProg map[string]*CommandStat, byAgent map[string]*AgentStat, bySource map[string]*SourceStat) {
	sum.Commands += a.Totals.Count
	sum.RawBytes += a.Totals.RawBytes
	sum.EmittedBytes += a.Totals.EmittedBytes
	sum.SavedBytes += a.Totals.SavedBytes
	for name, st := range a.Programs {
		cur := byProg[name]
		if cur == nil {
			cur = &CommandStat{Program: name}
			byProg[name] = cur
		}
		cur.Count += st.Count
		cur.RawBytes += st.RawBytes
		cur.EmittedBytes += st.EmittedBytes
		cur.SavedBytes += st.SavedBytes
	}
	for name, st := range a.Agents {
		cur := byAgent[name]
		if cur == nil {
			cur = &AgentStat{Agent: name}
			byAgent[name] = cur
		}
		cur.Commands += st.Count
		cur.RawBytes += st.RawBytes
		cur.EmittedBytes += st.EmittedBytes
		cur.SavedBytes += st.SavedBytes
	}
	for name, st := range a.Sources {
		cur := bySource[name]
		if cur == nil {
			cur = &SourceStat{Source: name}
			bySource[name] = cur
		}
		cur.Commands += st.Count
		cur.RawBytes += st.RawBytes
		cur.EmittedBytes += st.EmittedBytes
		cur.SavedBytes += st.SavedBytes
	}
}
