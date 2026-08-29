package w9y

// mod_tui.go — the interactive `w9y mod apply` terminal UI.
//
// Ported from the Bubble Tea package-manager / progress-bar examples
// (charm.land/bubbletea/v2, charm.land/bubbles/v2): a spinner, an
// overall progress bar (entries installed / total) and a live byte
// counter for the entry currently downloading. The download loop itself
// is the SAME applySources() the plain path uses — the TUI is only a
// renderer fed by the observe() hook, so headless behavior is unchanged.
// Active only when stdout is a terminal (or --progress=tui); in wasm /
// pipes / CI the plain output path runs.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// runApplyTUI runs the apply loop under a Bubble Tea program. The loop
// runs in a goroutine, reporting through prog; the model ticks a few
// times a second to re-render and quits when the loop finishes or the
// user cancels (which aborts the in-flight request via ctx).
func runApplyTUI(parent context.Context, host string, client *http.Client, sources []manifestSource, prefix string, verbose bool) error {
	ctx, cancel := context.WithCancel(parent)
	prog := &applyProgress{total: countApplyEntries(sources)}
	result := make(chan applySourcesResult, 1)
	go func() {
		res := applySources(ctx, host, client, sources, prefix, false, verbose, prog.observe)
		result <- res
	}()

	final, err := tea.NewProgram(newApplyTUI(prog, result, cancel)).Run()
	if err != nil {
		cancel()
		return err
	}
	model := final.(applyTUI)
	if model.quitEarly {
		cancel()
		return fmt.Errorf("apply cancelled by user")
	}
	if len(model.res.allErrors) > 0 {
		for _, e := range model.res.allErrors {
			fmt.Fprintf(os.Stderr, "error: %v\n", e)
		}
		return fmt.Errorf("%d entries failed", len(model.res.allErrors))
	}
	return nil
}

// countApplyEntries parses each source once to learn the total entry
// count for the progress bar (applySources re-parses; cheap, in-memory).
func countApplyEntries(sources []manifestSource) int {
	n := 0
	for _, src := range sources {
		m, err := ParseManifest(src.label, src.data)
		if err == nil {
			n += len(m.Entries)
		}
	}
	return n
}

// --- shared download state (written by the apply goroutine, read by
// the model on each tick) ---

type finishedRow struct {
	label  string
	ok     bool
	cached bool
}

type applyProgress struct {
	mu         sync.Mutex
	total      int
	index      int // entries started so far
	doneOK     int
	doneFail   int
	current    string
	currentMod string
	curDone    int64
	curTotal   int64
	finished   []finishedRow
}

func (p *applyProgress) observe(e applyEntry, phase entryPhase, done, total int64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch phase {
	case entryStart:
		p.index++
		p.current = e.Label
		p.currentMod = e.Mod
		p.curDone, p.curTotal = 0, 0
	case entryProgress:
		p.curDone, p.curTotal = done, total
	case entryDone:
		if ok {
			p.doneOK++
		} else {
			p.doneFail++
		}
		p.finished = append(p.finished, finishedRow{label: e.Label, ok: ok, cached: e.Cached})
		if len(p.finished) > 30 {
			p.finished = p.finished[len(p.finished)-30:]
		}
		p.current = ""
		p.curDone, p.curTotal = 0, 0
	}
}

type applySnapshot struct {
	total      int
	index      int
	doneOK     int
	doneFail   int
	current    string
	currentMod string
	curDone    int64
	curTotal   int64
	finished   []finishedRow
}

func (p *applyProgress) snapshot() applySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := applySnapshot{
		total:      p.total,
		index:      p.index,
		doneOK:     p.doneOK,
		doneFail:   p.doneFail,
		current:    p.current,
		currentMod: p.currentMod,
		curDone:    p.curDone,
		curTotal:   p.curTotal,
		finished:   append([]finishedRow(nil), p.finished...),
	}
	return s
}

// --- Bubble Tea model ---

type progressTickMsg struct{}

type applyTUI struct {
	prog      *applyProgress
	result    chan applySourcesResult
	cancel    context.CancelFunc
	spinner   spinner.Model
	bar       progress.Model
	width     int
	res       applySourcesResult
	got       bool
	quitEarly bool
}

var (
	tuiCheckMark = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).SetString("✓")
	tuiXMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).SetString("✗")
	tuiNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("211"))
	tuiMuted     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func newApplyTUI(prog *applyProgress, result chan applySourcesResult, cancel context.CancelFunc) applyTUI {
	s := spinner.New()
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	bar := progress.New(
		progress.WithDefaultBlend(),
		progress.WithWidth(36),
		progress.WithoutPercentage(),
	)
	return applyTUI{prog: prog, result: result, cancel: cancel, spinner: s, bar: bar}
}

func (m applyTUI) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.tickProgress())
}

func (m applyTUI) tickProgress() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return progressTickMsg{} })
}

func (m applyTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.quitEarly = true
			m.cancel()
			return m, tea.Quit
		}
	case tea.InterruptMsg:
		m.quitEarly = true
		m.cancel()
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		var cmd tea.Cmd
		m.bar, cmd = m.bar.Update(msg)
		return m, cmd
	case progressTickMsg:
		select {
		case res := <-m.result:
			m.got = true
			m.res = res
			return m, tea.Quit
		default:
			frac := 0.0
			if m.prog.total > 0 {
				frac = float64(m.prog.index) / float64(m.prog.total)
			}
			return m, tea.Batch(m.bar.SetPercent(frac), m.tickProgress())
		}
	}
	return m, nil
}

func (m applyTUI) View() tea.View {
	if m.got {
		return tea.NewView(m.summary())
	}
	s := m.prog.snapshot()
	var lines []string

	// Finished entries (last rows on top of the current one).
	show := s.finished
	if len(show) > 6 {
		show = show[len(show)-6:]
	}
	for _, row := range show {
		mark := tuiXMark
		if row.ok {
			mark = tuiCheckMark
		}
		suffix := ""
		if row.ok && row.cached {
			suffix = tuiMuted.Render(" (cached)")
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", mark, row.label, suffix))
	}

	// Main line, package-manager style: spinner + Installing <name> +
	// overall bar + N/M.
	spin := m.spinner.View() + " "
	prog := m.bar.View()
	count := fmt.Sprintf(" %d/%d", s.index, s.total)
	current := "starting…"
	if s.current != "" {
		current = "Installing " + tuiNameStyle.Render(s.current)
	}
	avail := m.width - lipgloss.Width(spin+prog+count)
	if avail < 0 {
		avail = 0
	}
	info := lipgloss.NewStyle().MaxWidth(avail).Render(current)
	gap := 0
	if avail > lipgloss.Width(info) {
		gap = avail - lipgloss.Width(info)
	}
	lines = append(lines, spin+info+strings.Repeat(" ", gap)+prog+count)

	// Live byte counter for the entry being downloaded.
	if s.current != "" && s.curTotal > 0 {
		lines = append(lines, "  "+tuiMuted.Render(fmt.Sprintf("%s  %s / %s",
			s.current, humanBytes(s.curDone), humanBytes(s.curTotal))))
	}

	fail := ""
	if s.doneFail > 0 {
		fail = tuiXMark.Render(fmt.Sprintf(" %d failed", s.doneFail))
	}
	lines = append(lines, tuiMuted.Render(fmt.Sprintf("q/esc: cancel · %d done%s", s.doneOK, fail)))

	return tea.NewView(strings.Join(lines, "\n"))
}

func (m applyTUI) summary() string {
	var lines []string
	if m.res.allErrors == nil || len(m.res.allErrors) == 0 {
		lines = append(lines, fmt.Sprintf("%s Done! Installed %d entries.", tuiCheckMark, m.prog.snapshot().doneOK))
	} else {
		lines = append(lines, fmt.Sprintf("%s Finished with %d error(s).", tuiXMark, len(m.res.allErrors)))
		for _, e := range m.res.allErrors {
			lines = append(lines, "  "+tuiMuted.Render(e.Error()))
		}
	}
	return strings.Join(lines, "\n")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
