package dash

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/grid"
	"github.com/jrimmer/louisiane/internal/proto"
)

// ChatView is one channel of the chat tab: the owner's IRC-like view of
// everything said on the mesh.
type ChatView struct {
	Relay    string        `json:"relay"`
	Epoch    string        `json:"epoch"`
	Now      int64         `json:"now"`
	Room     string        `json:"room"` // "*", "direct" or a room name
	Channels []Channel     `json:"channels"`
	Events   []proto.Event `json:"events"` // oldest first
	CanSend  bool          `json:"can_send"`
	Agents   int           `json:"agents"`
}

type Channel struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Kind    string `json:"kind,omitempty"` // project, topic, quiet (nobody in it now)
	Msgs    int    `json:"msgs"`
	Members int    `json:"members,omitempty"`
}

// cseg is a run of chat text that may be clickable: its id becomes the
// cells' data-id ("go:URL" navigates, "reply:ID" starts a reply).
type cseg struct {
	text, style, id string
}

type cline []cseg

func (l cline) width() int {
	n := 0
	for _, s := range l {
		n += len([]rune(s.text))
	}
	return n
}

func drawLine(g *grid.Grid, x, y int, l cline, max int) {
	for _, s := range l {
		if max <= 0 {
			return
		}
		t := grid.Sanitize(s.text)
		n := min(len([]rune(t)), max)
		g.Text(x, y, t, s.style, n)
		if s.id != "" {
			g.Mark(x, y, n, 1, s.id)
		}
		x += n
		max -= n
	}
}

// nickStyles colour agents' names, IRC-style: each name keeps one colour.
var nickStyles = []string{"n1", "n2", "n3", "n4", "n5", "n6"}

func nickStyle(nick string) string {
	switch {
	case nick == "owner":
		return "me"
	case strings.HasPrefix(nick, "@"), nick == "relay":
		return "dim"
	}
	h := fnv.New32a()
	h.Write([]byte(nick))
	return nickStyles[h.Sum32()%uint32(len(nickStyles))]
}

// wrap breaks s into lines of at most w runes, at spaces when it can.
func wrap(s string, w int) []string {
	if w < 8 {
		w = 8
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		line := []rune{}
		for _, word := range strings.Fields(para) {
			wr := []rune(word)
			for len(wr) > w { // a word longer than the line
				if len(line) > 0 {
					out = append(out, string(line))
					line = line[:0]
				}
				out = append(out, string(wr[:w]))
				wr = wr[w:]
			}
			switch {
			case len(line) == 0:
				line = append(line, wr...)
			case len(line)+1+len(wr) <= w:
				line = append(append(line, ' '), wr...)
			default:
				out = append(out, string(line))
				line = append(line[:0], wr...)
			}
		}
		out = append(out, string(line))
	}
	return out
}

func fit(s string, w int, right bool) string {
	r := []rune(s)
	if len(r) > w {
		if right {
			return "…" + string(r[len(r)-w+1:])
		}
		return string(r[:w-1]) + "…"
	}
	pad := strings.Repeat(" ", w-len(r))
	if right {
		return pad + s
	}
	return s + pad
}

const maxRows = 120

// chatLines renders the channel's events as wrapped lines.
func chatLines(v ChatView, inner int) []cline {
	mixed := v.Room == "*" || v.Room == "direct"
	// Columns scale with the width, so narrow terminals keep room for text.
	nickWidth := max(12, min(20, inner/5))
	whereWidth := max(8, min(12, inner/8))
	var out []cline
	for _, ev := range v.Events {
		t := time.UnixMilli(ev.At).Format("15:04:05")
		head := cline{{text: t + " ", style: "dim"}}
		if ev.Type != proto.EventMsg {
			text, style := systemText(ev)
			for i, l := range wrap(text, inner-len(t)-5) {
				if i == 0 {
					out = append(out, append(append(cline{}, head...), cseg{text: "-!- ", style: "dim"}, cseg{text: l, style: style}))
				} else {
					out = append(out, cline{{text: strings.Repeat(" ", len(t)+5)}, {text: l, style: style}})
				}
			}
			continue
		}
		if mixed {
			where := "#" + ev.Room
			if ev.Room == "" {
				where = "→ " + ev.To
			}
			head = append(head, cseg{text: fit(where, whereWidth, false) + " ", style: "id"})
		}
		nick := ev.From
		if !mixed && ev.Room != proto.AllRoom {
			nick = strings.TrimPrefix(nick, v.Room+"/")
		}
		head = append(head, cseg{text: fit(nick, nickWidth, true), style: nickStyle(ev.From)}, cseg{text: " │ ", style: "frame"})
		indent := cline{{text: strings.Repeat(" ", head.width()-3)}, {text: " │ ", style: "frame"}}
		lead := ""
		leadStyle := "dim"
		switch {
		case ev.Ask:
			lead, leadStyle = "? ", "warn"
		case ev.ReplyTo != "":
			lead = "↪ "
		}
		textW := inner - head.width()
		body := wrap(ev.Body, textW-len([]rune(lead)))
		id := cseg{text: " " + ev.ID, style: "dim", id: "reply:" + ev.ID}
		for i, l := range body {
			var ln cline
			if i == 0 {
				ln = append(append(cline{}, head...), cseg{text: lead, style: leadStyle})
			} else {
				ln = append(append(cline{}, indent...), cseg{text: strings.Repeat(" ", len([]rune(lead)))})
			}
			ln = append(ln, cseg{text: l, style: textStyle(ev)})
			if i == len(body)-1 {
				if ln.width()+len(id.text) <= inner {
					ln = append(ln, id)
				} else {
					out = append(out, ln)
					ln = append(append(cline{}, indent...), cseg{text: ev.ID, style: id.style, id: id.id})
				}
			}
			out = append(out, ln)
		}
	}
	return out
}

func textStyle(ev proto.Event) string {
	if ev.From == "owner" {
		return "head"
	}
	return "text"
}

func systemText(ev proto.Event) (string, string) {
	switch ev.Type {
	case proto.EventJoined:
		return fmt.Sprintf("%s joined (%s)", ev.Member, ev.Body), "ok"
	case proto.EventLeft:
		return fmt.Sprintf("%s left (%s)", ev.Member, ev.Body), "warn"
	case proto.EventLeader:
		s := fmt.Sprintf("%s now leads %s (term %d, %s", ev.Member, ev.Project, ev.Term, ev.Action)
		if ev.Action == "handoff" || ev.Action == "take" {
			s += " by " + ev.From
		} else if ev.Body != "" {
			s += " " + ev.Body
		}
		return s + ")", "title"
	case proto.EventRoom:
		verb := "joined"
		if ev.Action == "leave" {
			verb = "left"
		}
		return fmt.Sprintf("%s %s #%s", ev.Member, verb, ev.Room), "dim"
	case proto.EventStatus:
		if ev.Body == "" {
			return ev.Member + " cleared its status", "dim"
		}
		return ev.Member + " is working on: " + oneLine(ev.Body), "dim"
	case proto.EventRefused:
		return fmt.Sprintf("refused %s → %s: %s", ev.From, ev.To, ev.Body), "bad"
	}
	return ev.Type + " " + ev.Body, "dim"
}

// RenderChat draws the chat tab at width cells.
func RenderChat(v ChatView, width int) *grid.Grid {
	width = max(MinWidth, min(width, DefaultWidth))
	inner := width - 4

	// The channel bar, wrapped over as many rows as it needs.
	var bar []cline
	cur := cline{}
	for _, c := range v.Channels {
		label := c.Label
		if c.Msgs > 0 {
			label += fmt.Sprintf(" %d", c.Msgs)
		}
		style := "link"
		switch {
		case c.Name == v.Room:
			label, style = "["+label+"]", "head"
		case c.Kind == "quiet":
			style = "dim"
		}
		s := cseg{text: label, style: style, id: "go:/chat?room=" + c.Name}
		if cur.width()+len([]rune(label))+2 > inner && len(cur) > 0 {
			bar = append(bar, cur)
			cur = cline{}
		}
		if len(cur) > 0 {
			cur = append(cur, cseg{text: "  "})
		}
		cur = append(cur, s)
	}
	bar = append(bar, cur)

	lines := chatLines(v, inner)
	if len(lines) > maxRows {
		lines = lines[len(lines)-maxRows:]
	}
	if len(lines) == 0 {
		lines = []cline{{{text: "nothing said here yet", style: "dim"}}}
	}

	h := 2 + len(bar) + 2 + len(lines) + 2 + 1
	g := grid.New(width, h)
	header(g, width, "chat", v.Relay, v.Epoch, fmt.Sprintf("agents %d", v.Agents))
	y := 2
	g.Box(0, y, width, len(bar)+2, "frame", false)
	g.Title(2, y, []grid.Seg{seg(" channels ", "title")})
	for i, l := range bar {
		drawLine(g, 2, y+1+i, l, inner)
	}
	y += len(bar) + 2
	title := "#" + v.Room
	switch v.Room {
	case "*":
		title = "everything"
	case "direct":
		title = "direct messages"
	}
	g.Box(0, y, width, len(lines)+2, "frame", false)
	g.Title(2, y, []grid.Seg{seg(" "+title+" ", "title")})
	for i, l := range lines {
		drawLine(g, 2, y+1+i, l, inner)
	}
	y += len(lines) + 2
	help := cline{{text: "click an id to reply · /msg ADDR text · /ask text · /q ADDR question · /reply ID text · ", style: "dim"},
		{text: "older: feed", style: "link", id: "go:/feed"}}
	if !v.CanSend {
		help = cline{{text: "read-only: sending as owner needs the dashboard login (relay --dash-password-hash) · ", style: "dim"},
			{text: "older: feed", style: "link", id: "go:/feed"}}
	}
	drawLine(g, 1, y, help, width-2)
	return g
}

// header draws row 0: the name, the tabs, the relay, and right-aligned
// detail. The tabs are links in the page; the terminal shows them as text.
func header(g *grid.Grid, width int, active, relay, epoch, right string) {
	x := g.Text(1, 0, "louisiane", "head", -1) + 2
	for _, tab := range []struct{ name, url string }{{"status", "/"}, {"chat", "/chat"}} {
		label, style := " "+tab.name+" ", "link"
		if tab.name == active {
			label, style = "["+tab.name+"]", "head"
		}
		n := len([]rune(label))
		g.Text(x, 0, label, style, -1)
		g.Mark(x, 0, n, 1, "go:"+tab.url)
		x += n + 1
	}
	g.Segs(x+1, 0, []grid.Seg{seg(relay, "text"), seg("  epoch "+epoch, "dim")}, width-x-len(right)-4)
	g.Right(width-2, 0, []grid.Seg{seg(right, "dim")})
}
