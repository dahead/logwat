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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
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

type config struct {
	rootDir   string
	patterns  []string // multiple globs, e.g. *.log,*.txt
	recursive bool
	maxLines  int
	utc       bool
}

type logLine struct {
	when time.Time
	rel  string
	text string
}

type tailState struct {
	offset sync.Map // string -> *atomic.Int64 for lock-free reads
}

func newTailState() *tailState { return &tailState{} }

func (t *tailState) get(p string) int64 {
	if v, ok := t.offset.Load(p); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func (t *tailState) set(p string, off int64) {
	v, _ := t.offset.LoadOrStore(p, &atomic.Int64{})
	v.(*atomic.Int64).Store(off)
}

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
	flushMsg   struct{}
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
			errStyle := m.styleText.Foreground(lipgloss.Color("#ff6b6b"))
			m.lines.append(errStyle.Render("[err] " + msg.Error()))
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

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleFsnotifyEvents(ctx, watcher, absRoot, cfg, state, p)
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
			state.set(f, fi.Size())
		}
	}
}

func handleFsnotifyEvents(ctx context.Context, watcher *fsnotify.Watcher, absRoot string, cfg config, state *tailState, p *tea.Program) {
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
				_ = readNewLines(ev.Name, absRoot, cfg, state, p)
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				state.set(ev.Name, 0)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			p.Send(errMsg(err))
		}
	}
}

func readNewLines(path, root string, cfg config, state *tailState, p *tea.Program) error {
	if !fileMatches(root, path, cfg) {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	start := state.get(path)
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	// Handle log rotation (file truncated)
	if fi.Size() < start {
		start = 0
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}

	rel, _ := filepath.Rel(root, path)
	rel = filepath.Clean(rel)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		t := time.Now()
		if cfg.utc {
			t = t.UTC()
		}
		p.Send(logLineMsg{when: t, rel: rel, text: scanner.Text()})
	}

	if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
		state.set(path, pos)
	}

	return scanner.Err()
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

	if len(cfg.patterns) == 0 {
		return true
	}

	base := filepath.Base(absPath)
	for _, pat := range cfg.patterns {
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
	}
	return false
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

func parseArgs() (config, error) {
	var recursive bool
	var maxLines int
	var utc bool
	flag.BoolVar(&recursive, "recursive", true, "watch directories recursively")
	flag.BoolVar(&recursive, "r", true, "watch directories recursively (shorthand)")
	flag.IntVar(&maxLines, "max-lines", 5000, "maximum lines to retain in the viewport")
	flag.BoolVar(&utc, "utc", false, "render timestamps in UTC")

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
	if len(cfg.patterns) == 0 {
		cfg.patterns = []string{"*.log", "*.txt"}
	}
	return cfg, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s <directory> [pattern_globs] [--recursive] [--max-lines N] [--utc]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(flag.CommandLine.Output(), "Examples:")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat /var/log \"*.log,*.txt\" --recursive --max-lines 10000")
		fmt.Fprintln(flag.CommandLine.Output(), "  logwat /var/log --utc")
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
	p := tea.NewProgram(m, tea.WithAltScreen())

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
