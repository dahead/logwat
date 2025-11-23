// Application that watches a folder (recursive optional) for text/logfiles (file filter optional)
// and shows every new line in these logs as a CLI output. Uses Bubble Tea for nice formatting
// and colored output.
//
// Example calls:
//   logwat C:\ProgramData\Microsoft\IntuneManagementExtension\Logs *.log --recursive
//   logwat C:\ProgramData\Microsoft\IntuneManagementExtension\Logs
// Example output:
// 21.11.2025 10:35:45	intunelogs\sessions.log		[LAST LINE FROM THIS FILE]
// 21.11.2025 10:35:47	intunelogs\user.log			[LAST LINE FROM THIS FILE]

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
)

type config struct {
	rootDir   string
	patterns  []string // multiple globs, e.g. *.log,*.txt
	recursive bool
	maxLines  int
	utc       bool
	winPollMs int
}

type logLine struct {
	when time.Time
	rel  string
	text string
}

type tailState struct {
	mu     sync.Mutex
	offset map[string]int64
}

func newTailState() *tailState               { return &tailState{offset: make(map[string]int64)} }
func (t *tailState) get(p string) int64      { t.mu.Lock(); defer t.mu.Unlock(); return t.offset[p] }
func (t *tailState) set(p string, off int64) { t.mu.Lock(); t.offset[p] = off; t.mu.Unlock() }

type model struct {
	vp           viewport.Model
	lines        *ringBuf
	styleTime    lipgloss.Style
	stylePath    lipgloss.Style
	styleText    lipgloss.Style
	cfg          config
	err          error
	flushDue     bool
	flushEvery   time.Duration
	pathColWidth int
}

type (
	logLineMsg logLine
	errMsg     error
)

func initialModel(cfg config) model {
	vp := viewport.New(0, 0)
	m := model{
		vp:         vp,
		lines:      newRingBuf(max(1, cfg.maxLines)),
		styleTime:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#5B8", Dark: "#5B8"}),
		stylePath:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#58F", Dark: "#8AD"}),
		styleText:  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#111", Dark: "#DDD"}),
		cfg:        cfg,
		flushEvery: 80 * time.Millisecond,
	}
	// Initial info lines
	abs, _ := filepath.Abs(cfg.rootDir)
	pat := strings.Join(cfg.patterns, ", ")
	rec := "no"
	if cfg.recursive {
		rec = "yes"
	}
	banner := fmt.Sprintf("logwat - watching: %s (recursive: %s, pattern: %s). Press Ctrl+C or q to quit", abs, rec, pat)
	m.lines.append(m.styleText.Render(banner))
	return m
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height
		m.vp.SetContent(strings.Join(m.lines.slice(), "\n"))
		return m, nil
	case tea.KeyMsg:
		// Allow quitting with Ctrl+C or 'q'
		if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case logLineMsg:
		l := logLine(msg)
		// Append formatted line or continuation
		m.lines.append(m.formatLine(l))
		// Schedule a debounced flush to the viewport
		m.flushDue = true
		return m, tea.Tick(m.flushEvery, func(time.Time) tea.Msg { return flushMsg{} })
	case flushMsg:
		if m.flushDue {
			m.vp.SetContent(strings.Join(m.lines.slice(), "\n"))
			m.vp.GotoBottom()
			m.flushDue = false
		}
		return m, nil
	case errMsg:
		// Append error line instead of replacing view
		if msg != nil {
			m.lines.append(m.styleText.Copy().Foreground(lipgloss.Color("#ff6b6b")).Render("[err] " + msg.Error()))
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

func (m model) View() string {
	// Errors are appended as lines; do not replace the view
	return m.vp.View()
}

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
	if runtime.GOOS == "windows" {
		// Show Windows-style backslashes
		p = strings.ReplaceAll(filepath.ToSlash(p), "/", "\\")
	}
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
		return indent + m.styleText.Render(text)
	}

	// Primary line: columns separated by two spaces
	return ts + " " + pStyled + "  " + m.styleText.Render(text)
}

// isContinuation returns true if a line looks like a continuation of the previous entry
func isContinuation(s string) bool {
	if s == "" {
		return false
	}
	ls := strings.ToLower(s)
	return strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") ||
		strings.HasPrefix(ls, "at ") || strings.HasPrefix(ls, "caused by") ||
		strings.HasPrefix(s, "...") || strings.HasPrefix(s, "| ")
}

// stripANSI removes ANSI escape sequences
func stripANSI(s string) string {
	// simple state machine to drop ESC [ ... m sequences
	b := make([]rune, 0, len(s))
	esc := false
	code := false
	for _, r := range s {
		switch {
		case !esc && r == 0x1b:
			esc = true
		case esc && !code && (r == '[' || r == '(' || r == ')'):
			code = true
		case esc && code:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				// end of CSI sequence
				esc = false
				code = false
			}
		default:
			if !esc {
				b = append(b, r)
			}
		}
	}
	return string(b)
}

// displayWidth returns the printable width, ignoring ANSI
func displayWidth(s string) int {
	return lipgloss.Width(stripANSI(s))
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

type flushMsg struct{}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func watchAndTail(cfg config, p *tea.Program) error {
	absRoot, err := filepath.Abs(cfg.rootDir)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// gather directories
	dirs := map[string]struct{}{}
	if cfg.recursive {
		_ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dirs[path] = struct{}{}
			}
			return nil
		})
	} else {
		dirs[absRoot] = struct{}{}
	}
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	for _, d := range dirList {
		_ = watcher.Add(d)
	}

	state := newTailState()

	// prime offsets so we only output NEW lines from now on
	prime := func(root string) {
		for _, f := range listMatchingFiles(root, cfg) {
			if fi, err := os.Stat(f); err == nil {
				state.set(f, fi.Size())
			}
		}
	}
	prime(absRoot)

	// read new data from file and send as messages
	readNew := func(path string) error {
		if !fileMatches(absRoot, path, cfg) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		start := state.get(path)
		if fi, err := f.Stat(); err == nil && fi.Size() < start {
			start = 0
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				rel, _ := filepath.Rel(absRoot, path)
				rel = filepath.Clean(rel)
				t := time.Now()
				if cfg.utc {
					t = t.UTC()
				}
				// keep raw text; formatting will normalize CR later
				p.Send(logLineMsg{when: t, rel: rel, text: strings.TrimRight(line, "\r\n")})
			}
			if errors.Is(err, io.EOF) {
				if pos, perr := f.Seek(0, io.SeekCurrent); perr == nil {
					state.set(path, pos)
				}
				return nil
			}
			if err != nil {
				return err
			}
		}
	}

	// fsnotify event loop
	go func() {
		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) {
					// new dir inside tree
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = watcher.Add(ev.Name)
						if cfg.recursive {
							prime(ev.Name)
						}
						continue
					}
				}
				// Treat Chmod as Write on Windows: some tools trigger CHMOD instead of WRITE
				if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || (runtime.GOOS == "windows" && ev.Has(fsnotify.Chmod)) {
					_ = readNew(ev.Name)
				}
				if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					state.set(ev.Name, 0)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				p.Send(errMsg(err))
			}
		}
	}()

	// On Windows, some file appends may not reliably emit WRITE events depending on the writer.
	if runtime.GOOS == "windows" {
		ms := cfg.winPollMs
		if ms <= 0 {
			ms = 700
		}
		ticker := time.NewTicker(time.Duration(ms) * time.Millisecond)
		go func() {
			for range ticker.C {
				for _, f := range listMatchingFiles(absRoot, cfg) {
					_ = readNew(f)
				}
			}
		}()
	}

	return nil
}

func fileMatches(root string, absPath string, cfg config) bool {
	rel, err := filepath.Rel(root, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	fi, err := os.Stat(absPath)
	if err != nil || fi.IsDir() {
		return false
	}
	base := filepath.Base(absPath)
	if len(cfg.patterns) == 0 {
		return true
	}
	for _, pat := range cfg.patterns {
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
	}
	return false
}

func listMatchingFiles(root string, cfg config) []string {
	var files []string
	if cfg.recursive {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if fileMatches(root, abs, cfg) {
				files = append(files, abs)
			}
			return nil
		})
		return files
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		abs, _ := filepath.Abs(p)
		if fileMatches(root, abs, cfg) {
			files = append(files, abs)
		}
	}
	return files
}

func parseArgs() (config, error) {
	var recursive bool
	var maxLines int
	var utc bool
	var pollMs int
	flag.BoolVar(&recursive, "recursive", false, "watch directories recursively")
	flag.BoolVar(&recursive, "r", false, "watch directories recursively (shorthand)")
	flag.IntVar(&maxLines, "max-lines", 5000, "maximum lines to retain in the viewport")
	flag.BoolVar(&utc, "utc", false, "render timestamps in UTC")
	flag.IntVar(&pollMs, "win-poll-ms", 700, "Windows fallback polling interval in milliseconds")
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		return config{}, fmt.Errorf("missing directory argument")
	}
	cfg := config{rootDir: args[0], recursive: recursive, maxLines: maxLines, utc: utc, winPollMs: pollMs}
	// Patterns: comma-separated globs in arg[1]; default to *.log,*.txt when omitted
	if len(args) >= 2 {
		raw := strings.Split(args[1], ",")
		for _, s := range raw {
			s = strings.TrimSpace(s)
			if s != "" {
				cfg.patterns = append(cfg.patterns, s)
			}
		}
	}
	if len(cfg.patterns) == 0 {
		cfg.patterns = []string{"*.log", "*.txt"}
	}
	return cfg, nil
}

func main() {

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s <directory> [pattern_globs] [--recursive] [--max-lines N] [--utc] [--win-poll-ms M]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(flag.CommandLine.Output(), "Examples:")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat C:/ProgramData/Microsoft/IntuneManagementExtension/Logs \"*.log,*.txt\" --recursive --max-lines 10000")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat C:/ProgramData/Microsoft/IntuneManagementExtension/Logs --utc")
	}

	// Show help and exit if no parameters are provided
	if len(os.Args) <= 1 {
		flag.Usage()
		os.Exit(0)
	}

	cfg, err := parseArgs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(cfg.rootDir); err != nil {
		fmt.Fprintln(os.Stderr, "Directory not accessible:", err)
		os.Exit(2)
	}

	m := initialModel(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if err := watchAndTail(cfg, p); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to start watcher:", err)
		os.Exit(1)
	}
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
