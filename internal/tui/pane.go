package tui

import (
	"fmt"
	"sync"

	"github.com/Ceinl/plumtree/tui-runtime/layout"
	"github.com/Ceinl/plumtree/tui-runtime/screen"
	"github.com/hinshun/vt10x"
)

// pane is the main region: a vt10x terminal emulator that ingests the active
// session's raw PTY stream and renders its cell grid into the screen buffer.
// Using an emulator (rather than forwarding bytes) is what lets a sidebar live
// alongside live terminal output — the host's clear/scroll/cursor escapes are
// contained to this grid instead of the whole screen.
type pane struct {
	mu     sync.Mutex
	term   vt10x.Terminal
	w, h   int
	lx, ly int // pane top-left in screen coords (set in Layout)

	// curX/curY/curVisible are the host cursor position within the pane,
	// captured at Render for the loop to place the real cursor after Flush.
	curX, curY int
	curVisible bool

	// onResize is called when the pane's size changes so the owner can resize
	// the remote PTY to match.
	onResize func(cols, rows int)

	style layout.Style
}

func newPane(onResize func(cols, rows int)) *pane {
	return &pane{
		term:     vt10x.New(vt10x.WithSize(80, 24)),
		w:        80,
		h:        24,
		onResize: onResize,
	}
}

// feed writes host output into the emulator.
func (p *pane) feed(b []byte) {
	p.mu.Lock()
	t := p.term
	p.mu.Unlock()
	_, _ = t.Write(b)
}

// reset clears the emulator (used when switching sessions).
func (p *pane) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.term = vt10x.New(vt10x.WithSize(p.w, p.h))
}

// --- layout.Component ---

func (p *pane) GetStyle() layout.Style          { return p.style }
func (p *pane) IsDirty() bool                   { return true } // emulator content changes freely
func (p *pane) MakeDirty()                      {}
func (p *pane) ClearDirty()                     {}
func (p *pane) SetParent(layout.Component)      {}

func (p *pane) Layout(x, y, w, h int) {
	p.mu.Lock()
	p.lx, p.ly = x, y
	resized := w != p.w || h != p.h
	if resized && w > 0 && h > 0 {
		p.w, p.h = w, h
		p.term.Resize(w, h)
	}
	cb := p.onResize
	p.mu.Unlock()
	if resized && cb != nil && w > 0 && h > 0 {
		cb(w, h)
	}
}

func (p *pane) Render(s *screen.Screen) {
	p.mu.Lock()
	t := p.term
	x0, y0, w, h := p.lx, p.ly, p.w, p.h
	p.mu.Unlock()

	t.Lock()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g := t.Cell(x, y)
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			s.Set(x0+x, y0+y, ch, vtColor(g.FG, true), vtColor(g.BG, false), "")
		}
	}
	cur := t.Cursor()
	vis := t.CursorVisible()
	t.Unlock()

	p.mu.Lock()
	p.curX, p.curY = x0+cur.X, y0+cur.Y
	p.curVisible = vis
	p.mu.Unlock()
}

// cursor reports where to place the real cursor (absolute screen coords).
func (p *pane) cursor() (x, y int, visible bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.curX, p.curY, p.curVisible
}

// vtColor maps a vt10x color to the screen buffer's ANSI string form. Empty
// string lets the screen use its configured default fg/bg.
func vtColor(c vt10x.Color, fg bool) string {
	switch {
	case c == vt10x.DefaultFG || c == vt10x.DefaultBG:
		return ""
	case c < 256:
		if fg {
			return fmt.Sprintf("\x1b[38;5;%dm", uint32(c))
		}
		return fmt.Sprintf("\x1b[48;5;%dm", uint32(c))
	default:
		// 24-bit RGB packed in the low 24 bits.
		r := (uint32(c) >> 16) & 0xff
		g := (uint32(c) >> 8) & 0xff
		b := uint32(c) & 0xff
		if fg {
			return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
		}
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b)
	}
}
