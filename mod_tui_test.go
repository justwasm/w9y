package w9y

import (
	"strings"
	"testing"
)

// The apply TUI is presentation-only: this test drives the shared
// progress state through the observe hook (the same calls applySources
// makes) and checks the rendered view carries the expected bits:
// spinner, current entry name, overall N/M count, done marks, summary.
func TestApplyTUIRendersProgress(t *testing.T) {
	prog := &applyProgress{total: 3}
	// entry 1 done ok
	prog.observe(applyEntry{Label: "examples/spinner", Mod: "tuimod"}, entryStart, 0, 0, false)
	prog.observe(applyEntry{Label: "examples/spinner", Mod: "tuimod"}, entryProgress, 512, 1024, false)
	prog.observe(applyEntry{Label: "examples/spinner", Mod: "tuimod"}, entryDone, 0, 0, true)
	// entry 2 downloading
	prog.observe(applyEntry{Label: "examples/simple", Mod: "tuimod"}, entryStart, 0, 0, false)
	prog.observe(applyEntry{Label: "examples/simple", Mod: "tuimod"}, entryProgress, 256000, 512000, false)

	m := newApplyTUI(prog, make(chan applySourcesResult), func() {})
	m.width = 100
	view := m.View().Content

	for _, want := range []string{
		"examples/spinner", // done row
		"✓",
		"examples/simple", // current entry
		"2/3",
		"250.0 KB / 500.0 KB", // live byte counter
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\nview:\n%s", want, view)
		}
	}

	// A failed entry renders the failure mark and count.
	prog.observe(applyEntry{Label: "examples/timer", Mod: "tuimod"}, entryStart, 0, 0, false)
	prog.observe(applyEntry{Label: "examples/timer", Mod: "tuimod"}, entryDone, 0, 0, false)
	m2 := newApplyTUI(prog, make(chan applySourcesResult), func() {})
	m2.width = 100
	view2 := m2.View().Content
	if !strings.Contains(view2, "✗") {
		t.Errorf("failed entry should render ✗\nview:\n%s", view2)
	}
	if !strings.Contains(view2, "1 failed") {
		t.Errorf("failed count missing\nview:\n%s", view2)
	}
}

func TestApplyTUISummary(t *testing.T) {
	prog := &applyProgress{total: 1}
	prog.observe(applyEntry{Label: "bin/gear", Mod: "gear"}, entryStart, 0, 0, false)
	prog.observe(applyEntry{Label: "bin/gear", Mod: "gear"}, entryDone, 0, 0, true)
	m := applyTUI{prog: prog, got: true, res: applySourcesResult{}}
	view := m.View().Content
	if !strings.Contains(view, "Done! Installed 1 entries.") {
		t.Errorf("summary missing done line\nview:\n%s", view)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 * 1 << 20, "5.0 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
