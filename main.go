// Application that watches a folder (recursive optional) for text/logfiles (file filter optional)
// and shows every new line in these logs as a CLI output. Uses Bubble Tea for nice formatting
// and colored output.
//
// Example calls:
//   logwat /var/log *.log --recursive
//   logwat /var/log
// Example output:
// 21.11.2025 10:35:45	intunelogs/sessions.log		[LAST LINE FROM THIS FILE]
// 21.11.2025 10:35:47	intunelogs/user.log			[LAST LINE FROM THIS FILE]

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
)

// small generic-less helper (Go 1.20 compatible) used for capacities
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type config struct {
	rootDir   string
	patterns  []string // multiple globs, e.g. *.log,*.txt
	recursive bool
	maxLines  int
	utc       bool
	// include/exclude globs evaluated against relative path first; if empty,
	// include defaults to patterns or ["*.log","*.txt"]. Excludes override includes.
	include []string
	exclude []string
}

type logLine struct {
	when time.Time
	rel  string
	text string
}

type fileState struct {
	mu    sync.Mutex
	inode uint64
	dev   uint64
	off   int64
}

type tailState struct {
	// path -> *fileState
	meta sync.Map
}

func newTailState() *tailState { return &tailState{} }

// getOffset decides the starting offset considering inode/dev and truncation.
// If the stored inode differs from current, it resets offset to 0.
func (t *tailState) getOffset(path string, curInode, curDev uint64, curSize int64) int64 {
	v, _ := t.meta.LoadOrStore(path, &fileState{})
	fs := v.(*fileState)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// inode change => rotated/recreated file at same path
	if (fs.inode != 0 && (fs.inode != curInode || fs.dev != curDev)) || (curSize < fs.off) {
		fs.inode, fs.dev, fs.off = curInode, curDev, 0
		return 0
	}
	// first time we see file
	if fs.inode == 0 && (curInode != 0 || curDev != 0) {
		fs.inode, fs.dev = curInode, curDev
	}
	return fs.off
}

func (t *tailState) setOffset(path string, curInode, curDev uint64, off int64) {
	v, _ := t.meta.LoadOrStore(path, &fileState{})
	fs := v.(*fileState)
	fs.mu.Lock()
	fs.inode, fs.dev, fs.off = curInode, curDev, off
	fs.mu.Unlock()
}

type model struct {
	vp           viewport.Model
	styleTime    lipgloss.Style
	stylePath    lipgloss.Style
	styleText    lipgloss.Style
	styleInfo    lipgloss.Style
	styleWarn    lipgloss.Style
	styleError   lipgloss.Style
	cfg          config
	err          error
	flushDue     bool
	flushEvery   time.Duration
	pathColWidth int

	// Grouping & entries
	pending   map[string]*pendingGroup // key: relative path
	groupIdle time.Duration            // flush pending after idle
	expandAll bool                     // toggle expansion of groups

	// ring buffer of structured entries for rendering
	entriesCap  int
	entries     []entry
	entriesHead int
	entriesSize int
	// dedup tracking (ignoring timestamp): last emitted key and count
	lastKeyNoTS string

	// precompiled highlight regexes
	reErr  *regexp.Regexp
	reWarn *regexp.Regexp
	reInfo *regexp.Regexp

	// --- Filtering & Search ---
	filterEditing bool           // when true, capture keystrokes into filterEdit
	filterEdit    string         // current edit buffer
	filterActive  string         // applied filter string (substring or regex literal)
	filterIsRegex bool           // true if filterActive is a valid regex
	filterRe      *regexp.Regexp // compiled regex when filterIsRegex
	filterErr     string         // last regex compile error (for header)

	visibleLines  []string // last rendered, after filtering
	matchLineIdxs []int    // line indices in visibleLines matching search
	curMatch      int      // current selected match index in matchLineIdxs
}

type (
	logLineMsg logLine
	errMsg     error
	flushMsg   struct{}
)

func initialModel(cfg config) model {
	vp := viewport.New(0, 0)
	m := model{
		vp:         vp,
		styleTime:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#5B8", Dark: "#5B8"}),
		stylePath:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#58F", Dark: "#8AD"}),
		styleText:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#111", Dark: "#DDD"}),
		styleInfo:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#222", Dark: "#DDD"}),
		styleWarn:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#C77D00", Dark: "#FFB020"}),
		styleError: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#C00000", Dark: "#FF6B6B"}),
		cfg:        cfg,
		flushEvery: 80 * time.Millisecond,
		pending:    make(map[string]*pendingGroup),
		groupIdle:  350 * time.Millisecond,
		expandAll:  false,
		entriesCap: max(1, cfg.maxLines),
	}
	// Compile highlight regexes (case-insensitive word boundaries where applicable)
	m.reErr = regexp.MustCompile(`(?i)\b(error|failed|fail|panic|fatal|exception)\b`)
	m.reWarn = regexp.MustCompile(`(?i)\b(warn|warning|degrad|slow|timeout)\b`)
	m.reInfo = regexp.MustCompile(`(?i)\b(info|started|listening|ready)\b`)
	// Initial info lines
	abs, _ := filepath.Abs(cfg.rootDir)
	pat := strings.Join(cfg.patterns, ", ")
	rec := "no"
	if cfg.recursive {
		rec = "yes"
	}
	banner := fmt.Sprintf("logwat - watching: %s (recursive: %s, pattern: %s). Keys: / filter, Enter apply, Esc clear, n/N next/prev, e expand, q quit", abs, rec, pat)
	m.appendEntryWithDedup(entry{when: time.Now(), rel: "info", lines: []string{banner}})
	// Legend
	legend := "Legend: " + m.styleInfo.Render("INFO") + "  " + m.styleWarn.Render("WARN") + "  " + m.styleError.Render("ERROR")
	m.appendEntryWithDedup(entry{when: time.Now(), rel: "info", lines: []string{legend}})
	return m
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.vp.Width = msg.Width
		// Reserve one line for header
		h := msg.Height - 1
		if h < 1 {
			h = 1
		}
		m.vp.Height = h
		m.rebuildViewport()
		return m, nil
	case tea.KeyMsg:
		// Allow quitting with Ctrl+C or 'q'
		if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
			return m, tea.Quit
		}
		// Filter input mode handling
		if m.filterEditing {
			switch msg.Type {
			case tea.KeyEnter:
				// Apply filter
				m.applyFilter(m.filterEdit)
				m.filterEditing = false
				m.rebuildViewport()
				return m, nil
			case tea.KeyEsc:
				// Clear filter and exit editing
				m.filterEdit = ""
				m.applyFilter("")
				m.filterEditing = false
				m.rebuildViewport()
				return m, nil
			case tea.KeyBackspace:
				if len(m.filterEdit) > 0 {
					m.filterEdit = m.filterEdit[:len(m.filterEdit)-1]
					// live update
					m.applyFilter(m.filterEdit)
					m.rebuildViewport()
				}
				return m, nil
			default:
				s := msg.String()
				if s == "backspace" { // handle alternate backspace code on some terms
					if len(m.filterEdit) > 0 {
						m.filterEdit = m.filterEdit[:len(m.filterEdit)-1]
						m.applyFilter(m.filterEdit)
						m.rebuildViewport()
					}
					return m, nil
				}
				// Ignore control keys that produce names like "up", "down", etc.
				if len(s) == 1 && s != "\x1b" {
					m.filterEdit += s
					// live update
					m.applyFilter(m.filterEdit)
					m.rebuildViewport()
				}
				return m, nil
			}
		}
		if msg.String() == "e" {
			// toggle expand/collapse of grouped entries
			m.expandAll = !m.expandAll
			m.rebuildViewport()
			return m, nil
		}
		if msg.String() == "/" { // start filter editing
			m.filterEditing = true
			// start with current active as base
			m.filterEdit = m.filterActive
			return m, nil
		}
		if msg.String() == "n" || msg.String() == "N" {
			if len(m.matchLineIdxs) > 0 {
				if msg.String() == "n" {
					m.curMatch = (m.curMatch + 1) % len(m.matchLineIdxs)
				} else {
					m.curMatch = (m.curMatch - 1 + len(m.matchLineIdxs)) % len(m.matchLineIdxs)
				}
				// scroll to the selected line
				line := m.matchLineIdxs[m.curMatch]
				// Move viewport to show that line roughly at top
				if line >= 0 {
					// Estimate YOffset bounds; clamp within content height
					m.vp.GotoTop()
					// viewport has methods to set YOffset via SetYOffset in newer versions; fallback by scrolling
					m.vp.SetYOffset(line)
				}
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case logLineMsg:
		l := logLine(msg)
		m.ingestLogLine(l)
		// Schedule a debounced flush to the viewport
		m.flushDue = true
		return m, tea.Tick(m.flushEvery, func(time.Time) tea.Msg { return flushMsg{} })
	case flushMsg:
		if m.flushDue {
			// Flush any idle pending groups
			m.flushIdleGroups()
			m.rebuildViewport()
			m.vp.GotoBottom()
			m.flushDue = false
		}
		return m, nil
	case errMsg:
		// Render internal errors in the same 3-column layout (time | path | text)
		if msg != nil {
			// Timestamp
			t := time.Now()
			if m.cfg.utc {
				t = t.UTC()
			}
			// Error as its own entry (eligible for dedup)
			errText := m.styleText.Foreground(lipgloss.Color("#ff6b6b")).Render(msg.Error())
			m.appendEntryWithDedup(entry{when: t, rel: "error", lines: []string{errText}, sev: SevError})
			m.flushDue = true
			return m, tea.Tick(m.flushEvery, func(time.Time) tea.Msg { return flushMsg{} })
		}
		return m, nil
	default:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
}

func (m *model) View() string {
	// Add a small header with filter/search status
	header := ""
	if m.filterEditing {
		header = "/" + m.filterEdit
	} else if m.filterActive != "" {
		mode := "substr"
		if m.filterIsRegex {
			mode = "regex"
		}
		header = fmt.Sprintf("filter(%s): %q  matches: %d", mode, m.filterActive, len(m.matchLineIdxs))
		if m.filterErr != "" {
			header += "  err: " + m.filterErr
		}
	} else {
		header = "Press / to filter, n/N to navigate matches, e to expand, q to quit"
	}
	hdr := m.styleInfo.Render(header)
	content := m.vp.View()
	return hdr + "\n" + content
}

// formatLine renders a single line in the 3-column layout. Used when expanding groups.
func (m *model) formatLine(l logLine) string {
	// Normalize line breaks and strip trailing CR
	text := strings.ReplaceAll(l.text, "\r", "")

	// Determine if this is a continuation line (heuristic)
	isCont := isContinuation(text)

	// Timestamp
	t := l.when
	if m.cfg.utc {
		t = t.UTC()
	}
	tsRaw := t.Format("02.01.2006 15:04:05") // fixed width
	ts := m.styleTime.Render(tsRaw)

	// Path column with fixed width padding
	p := l.rel
	// track maximum path width (without style)
	w := displayWidth(stripANSI(p))
	if w > m.pathColWidth {
		m.pathColWidth = w
	}
	pad := m.pathColWidth - w
	if pad < 0 {
		pad = 0
	}
	paddedPath := p + strings.Repeat(" ", pad)
	pStyled := m.stylePath.Render(paddedPath)

	// Build the line
	if isCont {
		// Continuation: indent under text column; no timestamp/path
		indent := strings.Repeat(" ", len(tsRaw)+1+m.pathColWidth+2)
		return indent + m.applySeverityStyle(text, m.detectSeverity(text))
	}

	// Primary line: columns separated by two spaces
	return ts + " " + pStyled + "  " + m.applySeverityStyle(text, m.detectSeverity(text))
}

// isContinuation returns true if a line looks like a continuation of the previous entry
func isContinuation(s string) bool {
	if s == "" {
		return false
	}
	return strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") ||
		strings.HasPrefix(s, "...") || strings.HasPrefix(s, "| ")
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences
func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// displayWidth returns the printable width, ignoring ANSI
func displayWidth(s string) int {
	return lipgloss.Width(stripANSI(s))
}

// --- Highlighting & severity ---

// severity represents the detected severity of a log line
type severity int

const (
	SevNone severity = iota
	SevInfo
	SevWarn
	SevError
)

// detectSeverity determines severity based on regex patterns and simple heuristics.
// Precedence: ERROR > WARN > INFO.
func (m *model) detectSeverity(text string) severity {
	// If regexes are not set (shouldn't happen), default to none
	if m.reErr != nil && m.reErr.MatchString(text) {
		return SevError
	}
	if m.reWarn != nil && m.reWarn.MatchString(text) {
		return SevWarn
	}
	if m.reInfo != nil && m.reInfo.MatchString(text) {
		return SevInfo
	}
	return SevNone
}

// applySeverityStyle colors the whole line by severity and highlights keywords.
func (m *model) applySeverityStyle(text string, sev severity) string {
	base := m.styleText
	switch sev {
	case SevError:
		base = m.styleError
	case SevWarn:
		base = m.styleWarn
	case SevInfo:
		base = m.styleInfo
	default:
		// keep default text style
	}
	// Emphasize matched tokens within the chosen color by bolding them
	emphasized := text
	if sev == SevError && m.reErr != nil {
		bold := m.styleError.Bold(true)
		emphasized = m.reErr.ReplaceAllStringFunc(emphasized, func(s string) string { return bold.Render(s) })
	} else if sev == SevWarn && m.reWarn != nil {
		bold := m.styleWarn.Bold(true)
		emphasized = m.reWarn.ReplaceAllStringFunc(emphasized, func(s string) string { return bold.Render(s) })
	} else if sev == SevInfo && m.reInfo != nil {
		bold := m.styleInfo.Bold(true)
		emphasized = m.reInfo.ReplaceAllStringFunc(emphasized, func(s string) string { return bold.Render(s) })
	}
	return base.Render(emphasized)
}

// normalizeLine performs a fast-path JSON parse to extract level/msg/ts.
// It returns possibly updated timestamp, normalized text, and severity.
func (m *model) normalizeLine(when time.Time, text string) (time.Time, string, severity) {
	s := strings.TrimSpace(text)
	if len(s) >= 2 && len(s) <= 10*1024 && s[0] == '{' && s[len(s)-1] == '}' {
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			// level
			sev := SevNone
			if lvl, ok := pickString(obj, "level", "lvl", "severity", "log.level", "lv"); ok {
				sev = levelToSeverity(lvl)
			}
			// message
			msg := text
			if mstr, ok := pickString(obj, "msg", "message", "log", "message_text"); ok && mstr != "" {
				msg = mstr
			}
			// timestamp
			t := when
			if v, ok := obj["ts"]; ok {
				if tt, ok := parseAnyTime(v); ok {
					t = tt
				}
			} else if v, ok := obj["time"]; ok {
				if tt, ok := parseAnyTime(v); ok {
					t = tt
				}
			} else if v, ok := obj["timestamp"]; ok {
				if tt, ok := parseAnyTime(v); ok {
					t = tt
				}
			}
			if sev == SevNone {
				sev = m.detectSeverity(msg)
			}
			return t, msg, sev
		}
	}
	// Fallback: severity by regex
	return when, text, m.detectSeverity(text)
}

// pickString returns the first string value found for provided keys.
func pickString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch vv := v.(type) {
			case string:
				return vv, true
			case fmt.Stringer:
				return vv.String(), true
			}
		}
	}
	return "", false
}

// levelToSeverity maps common level strings to severity
func levelToSeverity(level string) severity {
	l := strings.ToLower(strings.TrimSpace(level))
	switch l {
	case "error", "err", "fatal", "panic", "crit", "critical", "severe":
		return SevError
	case "warn", "warning":
		return SevWarn
	case "info", "information", "notice":
		return SevInfo
	default:
		return SevNone
	}
}

// parseAnyTime tries a few common formats or numbers (unix seconds/millis)
func parseAnyTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		// RFC3339
		if tt, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return tt, true
		}
		if tt, err := time.Parse(time.RFC3339, s); err == nil {
			return tt, true
		}
		// Common logfmt ts like 2006-01-02 15:04:05
		if tt, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			return tt, true
		}
		// Try to parse as integer
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return epochToTime(n)
		}
	case float64:
		// JSON numbers decode to float64
		n := int64(t)
		return epochToTime(n)
	case int64:
		return epochToTime(t)
	case int:
		return epochToTime(int64(t))
	}
	return time.Time{}, false
}

func epochToTime(n int64) (time.Time, bool) {
	// Heuristic: if too big, assume milliseconds
	if n > 1_000_000_000_000 {
		return time.Unix(0, n*int64(time.Millisecond)), true
	}
	if n > 3_000_000_000 { // seconds but in the future? still accept
		return time.Unix(n/1000, (n%1000)*int64(time.Millisecond)), true
	}
	return time.Unix(n, 0), true
}

// --- Grouping & entries ---

type pendingGroup struct {
	when   time.Time
	rel    string
	lines  []string // first line at index 0
	lastAt time.Time
	sev    severity
}

type entry struct {
	when     time.Time
	rel      string
	lines    []string // first line + continuations
	dupCount int      // number of extra duplicates beyond the first
	sev      severity
}

func (m *model) ingestLogLine(l logLine) {
	text := strings.ReplaceAll(l.text, "\r", "")
	cont := isContinuation(text)
	pg := m.pending[l.rel]
	if cont {
		if pg == nil {
			// treat as its own primary when no pending exists
			sev := m.detectSeverity(text)
			m.pending[l.rel] = &pendingGroup{when: l.when, rel: l.rel, lines: []string{text}, lastAt: time.Now(), sev: sev}
		} else {
			pg.lines = append(pg.lines, text)
			pg.lastAt = time.Now()
		}
		return
	}
	// primary line: emit previous pending for this path
	if pg != nil {
		m.emitGroup(*pg)
		delete(m.pending, l.rel)
	}
	// Normalize/parse JSON fast-path for primary lines
	normWhen, normText, sev := m.normalizeLine(l.when, text)
	// start new group
	m.pending[l.rel] = &pendingGroup{when: normWhen, rel: l.rel, lines: []string{normText}, lastAt: time.Now(), sev: sev}
}

func (m *model) flushIdleGroups() {
	if m.groupIdle <= 0 {
		return
	}
	now := time.Now()
	for rel, pg := range m.pending {
		if now.Sub(pg.lastAt) >= m.groupIdle {
			m.emitGroup(*pg)
			delete(m.pending, rel)
		}
	}
}

func (m *model) emitAllPending() {
	for rel, pg := range m.pending {
		m.emitGroup(*pg)
		delete(m.pending, rel)
	}
}

func (m *model) emitGroup(pg pendingGroup) {
	m.appendEntryWithDedup(entry{when: pg.when, rel: pg.rel, lines: append([]string{}, pg.lines...), sev: pg.sev})
}

func (m *model) entryKeyNoTS(e entry) string {
	// Use rel + lines to define identical entry ignoring timestamps
	return e.rel + "\n" + strings.Join(e.lines, "\n")
}

func (m *model) appendEntryWithDedup(e entry) {
	key := m.entryKeyNoTS(e)
	// Check last entry for dedup
	if m.entriesSize > 0 {
		last := m.entriesGet(m.entriesSize - 1)
		if m.entryKeyNoTS(last) == key {
			last.dupCount++
			m.entriesSet(m.entriesSize-1, last)
			return
		}
	}
	m.entriesPush(e)
}

func (m *model) entriesPush(e entry) {
	cap := m.entriesCap
	if cap <= 0 {
		return
	}
	if len(m.entries) == 0 {
		m.entries = make([]entry, cap)
	}
	idx := (m.entriesHead + m.entriesSize) % cap
	m.entries[idx] = e
	if m.entriesSize < cap {
		m.entriesSize++
	} else {
		m.entriesHead = (m.entriesHead + 1) % cap
	}
}

func (m *model) entriesGet(i int) entry {
	idx := (m.entriesHead + i) % m.entriesCap
	return m.entries[idx]
}

func (m *model) entriesSet(i int, e entry) {
	idx := (m.entriesHead + i) % m.entriesCap
	m.entries[idx] = e
}

func (m *model) rebuildViewport() {
	// Re-render the content from structured entries
	var out []string
	m.pathColWidth = m.pathColWidth // preserved across renders
	for i := 0; i < m.entriesSize; i++ {
		e := m.entriesGet(i)
		// Apply entry-level filtering: if filter is active and collapsed, we include entry
		// only if any of its lines match; in expanded mode we'll check per line below.
		// render entry according to expandAll
		if m.expandAll || len(e.lines) == 0 || len(e.lines) == 1 {
			// expanded: render first as primary, rest as continuations
			if len(e.lines) == 0 {
				continue
			}
			// first line
			if m.linePassesFilter(e.rel, e.lines[0]) {
				out = append(out, m.renderPrimaryLine(e.when, e.rel, e.lines[0], e.dupCount, e.sev))
			}
			// continuations
			for j := 1; j < len(e.lines); j++ {
				if m.linePassesFilter(e.rel, e.lines[j]) {
					out = append(out, m.renderContinuationLine(e.when, e.lines[j], e.sev))
				}
			}
		} else {
			// collapsed: only first line with indication
			first := e.lines[0]
			more := len(e.lines) - 1
			if more > 0 {
				first = fmt.Sprintf("%s  [+%d more]", first, more)
			}
			// include entry if any of its lines match
			if m.entryPassesFilter(e) {
				out = append(out, m.renderPrimaryLine(e.when, e.rel, first, e.dupCount, e.sev))
			}
		}
	}
	// compute search matches and highlight them if filterActive present
	m.visibleLines = out
	m.computeMatches()
	highlighted := make([]string, len(m.visibleLines))
	for i, ln := range m.visibleLines {
		highlighted[i] = m.highlightMatches(ln)
	}
	m.vp.SetContent(strings.Join(highlighted, "\n"))
}

// linePassesFilter returns true if no filter is active or the pair rel|text passes the filter
func (m *model) linePassesFilter(rel, text string) bool {
	if m.filterActive == "" {
		return true
	}
	hay := rel + "\t" + text
	if m.filterIsRegex && m.filterRe != nil {
		return m.filterRe.MatchString(hay)
	}
	// substring (case-insensitive)
	return strings.Contains(strings.ToLower(hay), strings.ToLower(m.filterActive))
}

func (m *model) entryPassesFilter(e entry) bool {
	if m.filterActive == "" {
		return true
	}
	for _, ln := range e.lines {
		if m.linePassesFilter(e.rel, ln) {
			return true
		}
	}
	return false
}

func (m *model) applyFilter(s string) {
	m.filterActive = s
	m.filterErr = ""
	m.filterIsRegex = false
	m.filterRe = nil
	// Regex mode when starts and ends with '/'
	if len(s) >= 2 && strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
		pat := s[1 : len(s)-1]
		re, err := regexp.Compile(pat)
		if err != nil {
			m.filterErr = err.Error()
		} else {
			m.filterIsRegex = true
			m.filterRe = re
		}
	}
	// reset match navigation
	m.curMatch = 0
}

func (m *model) computeMatches() {
	m.matchLineIdxs = m.matchLineIdxs[:0]
	if m.filterActive == "" || len(m.visibleLines) == 0 {
		return
	}
	for i, ln := range m.visibleLines {
		if m.matchLine(ln) {
			m.matchLineIdxs = append(m.matchLineIdxs, i)
		}
	}
	if m.curMatch >= len(m.matchLineIdxs) {
		m.curMatch = 0
	}
}

func (m *model) matchLine(line string) bool {
	if m.filterActive == "" {
		return false
	}
	// match on the rendered line (including styles stripped for search)
	plain := stripANSI(line)
	if m.filterIsRegex && m.filterRe != nil {
		return m.filterRe.MatchString(plain)
	}
	return strings.Contains(strings.ToLower(plain), strings.ToLower(m.filterActive))
}

func (m *model) highlightMatches(line string) string {
	if m.filterActive == "" {
		return line
	}
	// naive highlighting on plain text, then re-apply by replacing segments in the original
	// Since ANSI already present, we will apply a simple regex to the raw string; this is not perfect
	style := lipgloss.NewStyle().Reverse(true)
	if m.filterIsRegex && m.filterRe != nil {
		return m.filterRe.ReplaceAllStringFunc(line, func(s string) string { return style.Render(s) })
	}
	// substring case-insensitive highlight: find all occurrences
	needle := strings.ToLower(m.filterActive)
	if needle == "" {
		return line
	}
	// Walk through line and build result
	var b strings.Builder
	lower := strings.ToLower(line)
	i := 0
	for {
		j := strings.Index(lower[i:], needle)
		if j < 0 {
			b.WriteString(line[i:])
			break
		}
		j += i
		b.WriteString(line[:j][i:])
		b.WriteString(style.Render(line[j : j+len(needle)]))
		i = j + len(needle)
		if i >= len(line) {
			break
		}
	}
	return b.String()
}

func (m *model) renderPrimaryLine(t time.Time, rel, text string, dup int, sev severity) string {
	if m.cfg.utc {
		t = t.UTC()
	}
	tsRaw := t.Format("02.01.2006 15:04:05")
	ts := m.styleTime.Render(tsRaw)

	// Path column with fixed width padding
	p := rel
	w := displayWidth(stripANSI(p))
	if w > m.pathColWidth {
		m.pathColWidth = w
	}
	pad := m.pathColWidth - w
	if pad < 0 {
		pad = 0
	}
	paddedPath := p + strings.Repeat(" ", pad)
	pStyled := m.stylePath.Render(paddedPath)

	if dup > 0 {
		text = fmt.Sprintf("%s (x%d)", text, dup+1)
	}
	return ts + " " + pStyled + "  " + m.applySeverityStyle(text, sev)
}

func (m *model) renderContinuationLine(t time.Time, text string, sev severity) string {
	if m.cfg.utc {
		t = t.UTC()
	}
	tsRaw := t.Format("02.01.2006 15:04:05")
	indent := strings.Repeat(" ", len(tsRaw)+1+m.pathColWidth+2)
	return indent + m.applySeverityStyle(text, sev)
}

// ringBuf is a fixed-capacity ring buffer for strings
type ringBuf struct {
	buf  []string
	head int
	size int
}

func newRingBuf(capacity int) *ringBuf {
	return &ringBuf{buf: make([]string, capacity)}
}

func (r *ringBuf) append(s string) {
	if len(r.buf) == 0 {
		return
	}
	idx := (r.head + r.size) % len(r.buf)
	r.buf[idx] = s
	if r.size < len(r.buf) {
		r.size++
	} else {
		r.head = (r.head + 1) % len(r.buf)
	}
}

func (r *ringBuf) slice() []string {
	out := make([]string, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	return out
}

func watchAndTail(ctx context.Context, cfg config, p *tea.Program) error {
	absRoot, err := filepath.Abs(cfg.rootDir)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// Collect directories to watch
	dirs, err := collectDirs(absRoot, cfg.recursive)
	if err != nil {
		return err
	}

	for _, d := range dirs {
		if err := watcher.Add(d); err != nil {
			p.Send(errMsg(fmt.Errorf("failed to watch %s: %w", d, err)))
		}
	}

	state := newTailState()
	primeOffsets(absRoot, cfg, state)

	// Async pipeline: readers -> linesCh (bounded, with drop counter) -> parser -> UI
	linesCh := make(chan logLine, 4096)
	dropped := &atomic.Int64{}

	var wg sync.WaitGroup
	// Parser/forwarder goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case l := <-linesCh:
				p.Send(logLineMsg(l))
			case <-ticker.C:
				if n := dropped.Swap(0); n > 0 {
					// Emit a synthetic drop notice
					txt := fmt.Sprintf("[backpressure] dropped %s lines", strconv.FormatInt(n, 10))
					p.Send(logLineMsg{when: time.Now(), rel: "info", text: txt})
				}
			}
		}
	}()

	// fsnotify reader goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleFsnotifyEvents(ctx, watcher, absRoot, cfg, state, p, linesCh, dropped)
	}()

	// Wait for context cancellation
	<-ctx.Done()
	wg.Wait()
	return nil
}

func collectDirs(root string, recursive bool) ([]string, error) {
	dirs := make(map[string]struct{})
	if recursive {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if d.IsDir() {
				dirs[path] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		dirs[root] = struct{}{}
	}

	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	return dirList, nil
}

func primeOffsets(root string, cfg config, state *tailState) {
	for _, f := range listMatchingFiles(root, cfg) {
		if fi, err := os.Stat(f); err == nil {
			ino, dev := inodeDev(fi)
			state.setOffset(f, ino, dev, fi.Size())
		}
	}
}

func handleFsnotifyEvents(ctx context.Context, watcher *fsnotify.Watcher, absRoot string, cfg config, state *tailState, p *tea.Program, out chan<- logLine, dropped *atomic.Int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			// fmt.Fprintf(os.Stderr, "DEBUG EVENT: %s %s\n", ev.Op, ev.Name)
			// Use bitwise operations instead of deprecated .Has()
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = watcher.Add(ev.Name)
					if cfg.recursive {
						primeOffsets(ev.Name, cfg, state)
					}
					continue
				}
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				_ = readNewLines(ctx, ev.Name, absRoot, cfg, state, out, dropped)
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// reset stored state for this path
				state.setOffset(ev.Name, 0, 0, 0)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			p.Send(errMsg(err))
		}
	}
}

func readNewLines(ctx context.Context, path, root string, cfg config, state *tailState, out chan<- logLine, dropped *atomic.Int64) error {
	if !fileMatches(root, path, cfg) {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	ino, dev := inodeDev(fi)
	start := state.getOffset(path, ino, dev, fi.Size())

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}

	rel, _ := filepath.Rel(root, path)
	rel = filepath.Clean(rel)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		t := time.Now()
		if cfg.utc {
			t = t.UTC()
		}
		l := logLine{when: t, rel: rel, text: scanner.Text()}
		select {
		case out <- l:
		default:
			dropped.Add(1)
		}
	}

	if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
		state.setOffset(path, ino, dev, pos)
	}

	return scanner.Err()
}

// inodeDev returns inode and device id for Linux/Unix platforms; on unsupported
// systems it returns zeros, which will degrade to path-only tracking.
func inodeDev(fi os.FileInfo) (uint64, uint64) {
	if fi == nil {
		return 0, 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), uint64(st.Dev)
	}
	return 0, 0
}

func fileMatches(root, absPath string, cfg config) bool {
	rel, err := filepath.Rel(root, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}

	fi, err := os.Stat(absPath)
	if err != nil || fi.IsDir() {
		return false
	}

	// Determine include set: cfg.include if set, else cfg.patterns, else defaults
	includes := cfg.include
	if len(includes) == 0 {
		includes = cfg.patterns
	}
	if len(includes) == 0 {
		includes = []string{"*.log", "*.txt"}
	}

	// Normalize rel for matching
	rel = filepath.ToSlash(rel)
	base := filepath.Base(absPath)
	matched := false
	for _, pat := range includes {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		// Try match against rel first, then base
		if ok, _ := filepath.Match(pat, rel); ok {
			matched = true
			break
		}
		if ok, _ := filepath.Match(pat, base); ok {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// Excludes: if any matches rel or base, reject
	for _, pat := range cfg.exclude {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return false
		}
		if ok, _ := filepath.Match(pat, base); ok {
			return false
		}
	}
	return true
}

func listMatchingFiles(root string, cfg config) []string {
	var files []string
	walkFn := func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if abs, err := filepath.Abs(path); err == nil && fileMatches(root, abs, cfg) {
			files = append(files, abs)
		}
		return nil
	}

	if cfg.recursive {
		_ = filepath.WalkDir(root, walkFn)
	} else {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() {
				p := filepath.Join(root, e.Name())
				_ = walkFn(p, e, nil)
			}
		}
	}
	return files
}

// computeMaxRelPathWidth inspects all currently matching files and returns the
// maximum printable width of their relative paths. This helps to establish a
// stable initial width for the path column so early lines don't look too short.
func computeMaxRelPathWidth(root string, cfg config) int {
	maxW := 0
	files := listMatchingFiles(root, cfg)
	for _, abs := range files {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		rel = filepath.Clean(rel)
		w := displayWidth(stripANSI(rel))
		if w > maxW {
			maxW = w
		}
	}
	return maxW
}

func parseArgs() (config, error) {
	var recursive bool
	var maxLines int
	var utc bool
	var includeArg string
	var excludeArg string
	flag.BoolVar(&recursive, "recursive", true, "watch directories recursively")
	flag.BoolVar(&recursive, "r", true, "watch directories recursively (shorthand)")
	flag.IntVar(&maxLines, "max-lines", 5000, "maximum lines to retain in the viewport")
	flag.BoolVar(&utc, "utc", false, "render timestamps in UTC")
	flag.StringVar(&includeArg, "include", "", "comma-separated include globs (match rel path or base)")
	flag.StringVar(&excludeArg, "exclude", "", "comma-separated exclude globs (match rel path or base)")

	// Show help if no args
	if len(os.Args) <= 1 {
		flag.Usage()
		os.Exit(0)
	}

	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		return config{}, fmt.Errorf("missing directory argument")
	}

	cfg := config{
		rootDir:   args[0],
		recursive: recursive,
		maxLines:  maxLines,
		utc:       utc,
	}

	if len(args) >= 2 {
		for _, s := range strings.Split(args[1], ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.patterns = append(cfg.patterns, s)
			}
		}
	}
	if includeArg != "" {
		for _, s := range strings.Split(includeArg, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.include = append(cfg.include, s)
			}
		}
	}
	if excludeArg != "" {
		for _, s := range strings.Split(excludeArg, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.exclude = append(cfg.exclude, s)
			}
		}
	}
	// patterns remain supported for backward compat and initial banner
	if len(cfg.patterns) == 0 && len(cfg.include) == 0 {
		cfg.patterns = []string{"*.log", "*.txt"}
	}
	return cfg, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s <directory> [pattern_globs] [--recursive] [--max-lines N] [--utc] [--include globs] [--exclude globs]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(flag.CommandLine.Output(), "Examples:")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat /var/log \"*.log,*.txt\" --recursive --max-lines 10000")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat /var/log --utc")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat /var/log --include \"**/*.log,*.txt\" --exclude \"**/archive/*,*.bak\"")
	}

	cfg, err := parseArgs() // Remove the os.Args check from here
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		flag.Usage()
		os.Exit(2)
	}

	if _, err := os.Stat(cfg.rootDir); err != nil {
		fmt.Fprintln(os.Stderr, "Directory not accessible:", err)
		os.Exit(2)
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	m := initialModel(cfg)
	p := tea.NewProgram(&m, tea.WithAltScreen())

	// Start watcher in background
	var watchErr error
	watchDone := make(chan struct{})
	go func() {
		watchErr = watchAndTail(ctx, cfg, p)
		close(watchDone)
	}()

	// Run the UI
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		cancel()
		<-watchDone
		os.Exit(1)
	}

	// Graceful shutdown
	cancel()
	<-watchDone

	if watchErr != nil && !errors.Is(watchErr, context.Canceled) {
		fmt.Fprintln(os.Stderr, "Watcher error:", watchErr)
		os.Exit(1)
	}
}
