// Package mountinfo parses /proc/self/mountinfo entries.
package mountinfo

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Entry represents a single parsed line of /proc/self/mountinfo.
type Entry struct {
	MountID, ParentID int
	Major, Minor      int
	Root, MountPoint  string // unescaped
	Options           string
	FSType, Source    string
	SuperOptions      string
}

// Dev returns the kernel's internal device number of the mount
// ((major << 20) | minor), which names its fusectl connection directory.
func (e Entry) Dev() uint64 { return uint64(e.Major)<<20 | uint64(e.Minor) }

// Parse reads mountinfo entries from r. A malformed line produces an error
// of the form "mountinfo: line %d: %s".
func Parse(r io.Reader) ([]Entry, error) {
	var entries []Entry
	scanner := bufio.NewScanner(r)
	// Super options can be long (e.g. overlay lowerdir lists): a line over
	// the default 64KiB token limit must not fail the whole read.
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("mountinfo: line %d: %s", lineNum, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mountinfo: %w", err)
	}
	return entries, nil
}

func parseLine(line string) (Entry, error) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return Entry{}, fmt.Errorf("too few fields (%d)", len(fields))
	}

	sepIdx := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 {
		return Entry{}, fmt.Errorf("missing separator")
	}
	rest := fields[sepIdx+1:]
	if len(rest) < 2 {
		return Entry{}, fmt.Errorf("too few fields after separator (%d)", len(rest))
	}

	mountID, err := strconv.Atoi(fields[0])
	if err != nil {
		return Entry{}, fmt.Errorf("invalid mount ID %q", fields[0])
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return Entry{}, fmt.Errorf("invalid parent ID %q", fields[1])
	}

	majorStr, minorStr, ok := strings.Cut(fields[2], ":")
	if !ok {
		return Entry{}, fmt.Errorf("invalid major:minor %q", fields[2])
	}
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid major %q", majorStr)
	}
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid minor %q", minorStr)
	}

	e := Entry{
		MountID:    mountID,
		ParentID:   parentID,
		Major:      major,
		Minor:      minor,
		Root:       Unescape(fields[3]),
		MountPoint: Unescape(fields[4]),
		Options:    fields[5],
		FSType:     rest[0],
		Source:     rest[1],
	}
	if len(rest) >= 3 {
		e.SuperOptions = rest[2]
	}
	return e, nil
}

// Read reads and parses /proc/self/mountinfo.
func Read() ([]Entry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Top returns the topmost mount stacked at path: the entry at path that is
// not the parent of another entry at path. List order is not stacking order
// (a mount can be moved on top of a newer one, or mounted beneath an older
// one), so the parent links decide. When they do not single one out, the
// last such entry in the list is returned.
func Top(entries []Entry, path string) (Entry, bool) {
	parents := make(map[int]bool)
	found := false
	for _, e := range entries {
		if e.MountPoint == path {
			found = true
			if e.ParentID != e.MountID {
				parents[e.ParentID] = true
			}
		}
	}
	if !found {
		return Entry{}, false
	}
	last := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].MountPoint != path {
			continue
		}
		if last < 0 {
			last = i
		}
		if !parents[entries[i].MountID] {
			return entries[i], true
		}
	}
	return entries[last], true
}

// Unescape decodes the octal escapes used by mountinfo for mount points and
// roots: \040 (space), \011 (tab), \012 (newline), \134 (backslash).
func Unescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			switch s[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 3
				continue
			case "011":
				b.WriteByte('\t')
				i += 3
				continue
			case "012":
				b.WriteByte('\n')
				i += 3
				continue
			case "134":
				b.WriteByte('\\')
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
