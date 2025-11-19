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
    pattern   string // e.g. *.log
    recursive bool
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

func newTailState() *tailState { return &tailState{offset: make(map[string]int64)} }
func (t *tailState) get(p string) int64 { t.mu.Lock(); defer t.mu.Unlock(); return t.offset[p] }
func (t *tailState) set(p string, off int64) { t.mu.Lock(); t.offset[p] = off; t.mu.Unlock() }

type model struct {
    vp        viewport.Model
    lines     []string
    styleTime lipgloss.Style
    stylePath lipgloss.Style
    styleText lipgloss.Style
    cfg       config
    err       error
}

type (
    logLineMsg logLine
    errMsg     error
)

func initialModel(cfg config) model {
    vp := viewport.New(0, 0)
    return model{
        vp:        vp,
        lines:     []string{},
        styleTime: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#5B8", Dark: "#5B8"}),
        stylePath: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#58F", Dark: "#8AD"}),
        styleText: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#111", Dark: "#DDD"}),
        cfg:       cfg,
    }
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.WindowSizeMsg:
        m.vp.Width = msg.Width
        m.vp.Height = msg.Height
        m.vp.SetContent(strings.Join(m.lines, "\n"))
        return m, nil
    case logLineMsg:
        l := logLine(msg)
        m.lines = append(m.lines, m.formatLine(l))
        if len(m.lines) > 5000 {
            m.lines = m.lines[len(m.lines)-5000:]
        }
        m.vp.SetContent(strings.Join(m.lines, "\n"))
        m.vp.GotoBottom()
        return m, nil
    case errMsg:
        m.err = msg
        return m, nil
    default:
        var cmd tea.Cmd
        m.vp, cmd = m.vp.Update(msg)
        return m, cmd
    }
}

func (m model) View() string {
    if m.err != nil {
        return "Error: " + m.err.Error() + "\n" + m.vp.View()
    }
    return m.vp.View()
}

func (m model) formatLine(l logLine) string {
    ts := m.styleTime.Render(l.when.Format("02.01.2006 15:04:05"))
    p := l.rel
    if runtime.GOOS == "windows" {
        // Show Windows-style backslashes as in the example
        p = strings.ReplaceAll(filepath.ToSlash(p), "/", "\\")
    }
    p = m.stylePath.Render(p)
    txt := m.styleText.Render(l.text)
    return fmt.Sprintf("%s\t%s\t\t%s", ts, p, txt)
}

func watchAndTail(cfg config, p *tea.Program) error {
    absRoot, err := filepath.Abs(cfg.rootDir)
    if err != nil { return err }

    watcher, err := fsnotify.NewWatcher()
    if err != nil { return err }

    // gather directories
    dirs := map[string]struct{}{}
    if cfg.recursive {
        _ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
            if err != nil { return nil }
            if d.IsDir() { dirs[path] = struct{}{} }
            return nil
        })
    } else {
        dirs[absRoot] = struct{}{}
    }
    dirList := make([]string, 0, len(dirs))
    for d := range dirs { dirList = append(dirList, d) }
    sort.Strings(dirList)
    for _, d := range dirList { _ = watcher.Add(d) }

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
        if !fileMatches(absRoot, path, cfg) { return nil }
        f, err := os.Open(path)
        if err != nil { return err }
        defer f.Close()
        start := state.get(path)
        if fi, err := f.Stat(); err == nil && fi.Size() < start { start = 0 }
        if _, err := f.Seek(start, io.SeekStart); err != nil { return err }
        r := bufio.NewReader(f)
        for {
            line, err := r.ReadString('\n')
            if len(line) > 0 {
                rel, _ := filepath.Rel(absRoot, path)
                rel = filepath.Clean(rel)
                p.Send(logLineMsg{when: time.Now(), rel: rel, text: strings.TrimRight(line, "\r\n")})
            }
            if errors.Is(err, io.EOF) {
                if pos, perr := f.Seek(0, io.SeekCurrent); perr == nil { state.set(path, pos) }
                return nil
            }
            if err != nil { return err }
        }
    }

    // fsnotify event loop
    go func() {
        for {
            select {
            case ev, ok := <-watcher.Events:
                if !ok { return }
                if ev.Has(fsnotify.Create) {
                    // new dir inside tree
                    if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
                        _ = watcher.Add(ev.Name)
                        if cfg.recursive { prime(ev.Name) }
                        continue
                    }
                }
                if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
                    _ = readNew(ev.Name)
                }
                if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
                    state.set(ev.Name, 0)
                }
            case err, ok := <-watcher.Errors:
                if !ok { return }
                p.Send(errMsg(err))
            }
        }
    }()

    return nil
}

func fileMatches(root string, absPath string, cfg config) bool {
    rel, err := filepath.Rel(root, absPath)
    if err != nil || strings.HasPrefix(rel, "..") { return false }
    fi, err := os.Stat(absPath)
    if err != nil || fi.IsDir() { return false }
    base := filepath.Base(absPath)
    pattern := cfg.pattern
    if pattern == "" {
        m1, _ := filepath.Match("*.log", base)
        m2, _ := filepath.Match("*.txt", base)
        return m1 || m2
    }
    ok, _ := filepath.Match(pattern, base)
    return ok
}

func listMatchingFiles(root string, cfg config) []string {
    var files []string
    if cfg.recursive {
        _ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
            if err != nil { return nil }
            if d.IsDir() { return nil }
            abs, _ := filepath.Abs(path)
            if fileMatches(root, abs, cfg) { files = append(files, abs) }
            return nil
        })
        return files
    }
    entries, _ := os.ReadDir(root)
    for _, e := range entries {
        if e.IsDir() { continue }
        p := filepath.Join(root, e.Name())
        abs, _ := filepath.Abs(p)
        if fileMatches(root, abs, cfg) { files = append(files, abs) }
    }
    return files
}

func parseArgs() (config, error) {
    var recursive bool
    flag.BoolVar(&recursive, "recursive", false, "watch directories recursively")
    flag.BoolVar(&recursive, "r", false, "watch directories recursively (shorthand)")
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s <folder> [pattern] [--recursive]\n", filepath.Base(os.Args[0]))
        fmt.Fprintln(flag.CommandLine.Output(), "Examples:")
        fmt.Fprintln(flag.CommandLine.Output(), "  logwat C:/ProgramData/Microsoft/IntuneManagementExtension/Logs *.log --recursive")
        fmt.Fprintln(flag.CommandLine.Output(), "  logwat C:/ProgramData/Microsoft/IntuneManagementExtension/Logs")
    }
    flag.Parse()
    args := flag.Args()
    if len(args) < 1 {
        return config{}, fmt.Errorf("missing folder argument")
    }
    cfg := config{rootDir: args[0], recursive: recursive}
    if len(args) >= 2 { cfg.pattern = args[1] }
    return cfg, nil
}

func main() {
    cfg, err := parseArgs()
    if err != nil {
        fmt.Fprintln(os.Stderr, "Error:", err)
        flag.Usage()
        os.Exit(2)
    }
    if _, err := os.Stat(cfg.rootDir); err != nil {
        fmt.Fprintln(os.Stderr, "Folder not accessible:", err)
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
