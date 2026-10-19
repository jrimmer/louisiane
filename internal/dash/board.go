package dash

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/louisiane/internal/grid"
)

//go:embed static page.html.tmpl
var assets embed.FS

var page = template.Must(template.ParseFS(assets, "page.html.tmpl"))

// Source is what the board draws from: the relay.
type Source interface {
	Snapshot() Snapshot
	Chat(room string, n int) ChatView
}

// Board serves the dashboard's two tabs (status and chat) and wakes every
// open page when something changes.
type Board struct {
	src     Source
	mu      sync.Mutex
	last    Snapshot
	viewers map[chan struct{}]bool
}

func NewBoard(src Source) *Board {
	return &Board{src: src, viewers: map[chan struct{}]bool{}}
}

// Update replaces the snapshot and nudges every page.
func (b *Board) Update(s Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = s
	for ch := range b.viewers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (b *Board) snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

const chatEvents = 300

// frame draws one view: "chat" (for room) or the status page.
func (b *Board) frame(view, room string) *grid.Grid {
	if view == "chat" {
		return RenderChat(b.src.Chat(room, chatEvents), DefaultWidth)
	}
	return Render(b.snapshot(), DefaultWidth)
}

// Handler serves the status tab at /, the chat tab at /chat, their stream
// at /stream and the assets under /static/.
func (b *Board) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /stream", b.handleStream)
	mux.HandleFunc("GET /chat", func(w http.ResponseWriter, r *http.Request) { b.servePage(w, "chat", room(r)) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { b.servePage(w, "status", "") })
	return mux
}

func room(r *http.Request) string {
	if v := r.URL.Query().Get("room"); v != "" {
		return strings.TrimPrefix(v, "#")
	}
	return "*"
}

func (b *Board) servePage(w http.ResponseWriter, view, room string) {
	g := b.frame(view, room)
	canSend := false
	if view == "chat" {
		canSend = b.src.Chat(room, 0).CanSend
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.Execute(w, map[string]any{
		"Grid":      template.HTML(pageGrid(g)),
		"View":      view,
		"Room":      room,
		"CanSend":   canSend,
		"StreamURL": fmt.Sprintf("/stream?view=%s&room=%s", view, template.URLQueryEscaper(room)),
	})
}

func pageRows(g *grid.Grid) []string { return strings.Split(g.HTML(), "\n") }

func pageGrid(g *grid.Grid) string {
	var sb strings.Builder
	for i, row := range pageRows(g) {
		fmt.Fprintf(&sb, `<span class="gr" id="r%d">%s</span>`, i, row)
	}
	return sb.String()
}

// handleStream sends Datastar element patches: one per row that changed
// since this page's last frame, or the whole grid when the row count
// changed.
func (b *Board) handleStream(w http.ResponseWriter, r *http.Request) {
	view, rm := r.URL.Query().Get("view"), room(r)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.viewers[ch] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.viewers, ch)
		b.mu.Unlock()
	}()
	var sent []string
	for {
		rows := pageRows(b.frame(view, rm))
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if len(rows) != len(sent) {
			var sb strings.Builder
			for i, row := range rows {
				fmt.Fprintf(&sb, `<span class="gr" id="r%d">%s</span>`, i, row)
			}
			if writeElements(w, "#grid", "inner", sb.String()) != nil {
				return
			}
		} else {
			for i, row := range rows {
				if row != sent[i] {
					if writeElements(w, fmt.Sprintf("#r%d", i), "inner", row) != nil {
						return
					}
				}
			}
		}
		sent = rows
		if rc.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		}
	}
}

func writeElements(w io.Writer, selector, mode, html string) error {
	if _, err := fmt.Fprintf(w, "event: datastar-patch-elements\ndata: selector %s\ndata: mode %s\n", selector, mode); err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimRight(html, "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "data: elements %s\n", l); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
