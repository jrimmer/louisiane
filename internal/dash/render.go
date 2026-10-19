// Package dash is louisiane's live view, built the way spoond's dashboard is:
// the relay builds a Snapshot, Render draws it on a character grid, and
// the same grid goes to browsers (one SSE stream per page, Datastar
// patching only the rows that changed) and to `lal top` as ANSI text.
package dash

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jrimmer/louisiane/internal/grid"
	"github.com/jrimmer/louisiane/internal/proto"
)

// Snapshot is everything the dashboard draws.
type Snapshot struct {
	Relay    string          `json:"relay"`
	Epoch    string          `json:"epoch"`
	Started  int64           `json:"started"`
	Now      int64           `json:"now"`
	Seq      uint64          `json:"seq"`
	Hosts    []HostSeen      `json:"hosts"`
	Projects []proto.Project `json:"projects"`
	Rooms    []proto.Room    `json:"rooms"`
	Events   []proto.Event   `json:"events"`   // newest last
	Refusals []proto.Event   `json:"refusals"` // newest last
	Rate     []float64       `json:"rate"`     // messages per bucket, oldest first
	Bucket   int             `json:"bucket"`   // seconds per Rate bucket
	PerMin   int             `json:"per_min"`  // messages in the last minute
}

type HostSeen struct {
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
	Members  int    `json:"members"`
}

// CheckPassword compares a password with a bcrypt hash.
func CheckPassword(hash, password string) bool {
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// HashPassword makes the bcrypt hash for --dash-password-hash.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

const (
	MinWidth     = 72
	DefaultWidth = 104
)

type line []grid.Seg

func seg(text, style string) grid.Seg { return grid.Seg{Text: grid.Sanitize(text), Style: style} }

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func clock(ms int64) string { return time.UnixMilli(ms).Format("15:04:05") }

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func short(member, project string) string {
	if rest, ok := strings.CutPrefix(member, project+"/"); ok {
		return rest
	}
	return member
}

// Render draws s at width cells.
func Render(s Snapshot, width int) *grid.Grid {
	width = max(MinWidth, min(width, DefaultWidth))
	inner := width - 4
	agents := 0
	for _, p := range s.Projects {
		agents += len(p.Members)
	}

	var panels []panel

	// Agents, by project.
	var al []line
	if len(s.Projects) == 0 {
		al = append(al, line{seg("nobody is connected", "dim")})
	}
	for i, p := range s.Projects {
		if i > 0 {
			al = append(al, line{})
		}
		al = append(al, line{seg(p.Name, "title"), seg(fmt.Sprintf("  term %d", p.Term), "dim"),
			seg("  "+p.Key, "dim")})
		for _, m := range p.Members {
			mark, ms := "●", "ok"
			if m.Streams == 0 {
				mark, ms = "○", "warn"
			}
			lead := "  "
			if m.Leader {
				lead = "◆ "
			}
			l := line{
				seg(" "+mark+" ", ms), seg(lead, "title"),
				seg(fmt.Sprintf("%-16s", short(m.Name, p.Name)), "owner"),
				seg(fmt.Sprintf("%-6s %-10s", m.Kind, m.Host), "state"),
				seg(fmt.Sprintf("%6s", dur(s.Now-m.ConnectedAt)), "dim"),
				seg(fmt.Sprintf("  ↑%-4d↓%-4d", m.Sent, m.Received), "text"),
			}
			if m.WorkingOn != "" {
				l = append(l, seg(oneLine(m.WorkingOn), "text"))
			}
			al = append(al, l)
		}
	}
	panels = append(panels, panel{title: fmt.Sprintf("agents %d", agents), lines: al})

	// Rooms.
	var rl []line
	for _, rm := range s.Rooms {
		if rm.Kind == "project" && rm.Msgs == 0 {
			continue
		}
		last := ""
		if rm.LastAt > 0 {
			last = dur(s.Now-rm.LastAt) + " ago"
		}
		name := "#" + rm.Name
		if rm.Name == "dm" && rm.Kind == "" {
			name = "direct"
		}
		in := fmt.Sprintf("%3d in", len(rm.Members))
		if rm.Kind == "" {
			in = "      "
		}
		l := line{seg(fmt.Sprintf("%-18s", name), "id"), seg(fmt.Sprintf("%s  %5d msgs  %8s  ", in, rm.Msgs, last), "dim")}
		if rm.Kind == "topic" {
			l = append(l, seg(strings.Join(rm.Members, " "), "owner"))
		}
		rl = append(rl, l)
	}
	panels = append(panels, panel{title: "rooms", lines: rl})

	// Traffic.
	spark := ""
	if len(s.Rate) > 0 {
		hi := 1.0
		for _, v := range s.Rate {
			hi = max(hi, v)
		}
		spark = grid.Sparkline(s.Rate, 0, hi)
	}
	panels = append(panels, panel{title: "traffic", lines: []line{{
		seg(spark, "spark"), seg(fmt.Sprintf("  %d/min  ", s.PerMin), "text"),
		seg(fmt.Sprintf("last %dm, %ds a bar", len(s.Rate)*s.Bucket/60, s.Bucket), "dim"),
	}}})

	// Events, newest first.
	var el []line
	for i := len(s.Events) - 1; i >= 0 && len(el) < 18; i-- {
		el = append(el, eventLine(s.Events[i]))
	}
	if len(el) == 0 {
		el = append(el, line{seg("quiet", "dim")})
	}
	panels = append(panels, panel{title: "events", lines: el})

	if len(s.Refusals) > 0 {
		var fl []line
		for i := len(s.Refusals) - 1; i >= 0 && len(fl) < 6; i-- {
			e := s.Refusals[i]
			fl = append(fl, line{seg(clock(e.At)+"  ", "dim"), seg(e.From, "owner"), seg(" → "+e.To+"  ", "dim"), seg(oneLine(e.Body), "bad")})
		}
		panels = append(panels, panel{title: "refused", lines: fl})
	}

	h := 2
	for _, p := range panels {
		h += len(p.lines) + 2
	}
	h++ // footer
	g := grid.New(width, h)
	right := fmt.Sprintf("up %s  hosts %d  agents %d  %d/min  #%d", dur(s.Now-s.Started), len(s.Hosts), agents, s.PerMin, s.Seq)
	header(g, width, "status", s.Relay, s.Epoch, right)
	y := 2
	for _, p := range panels {
		g.Box(0, y, width, len(p.lines)+2, "frame", false)
		g.Title(2, y, []grid.Seg{seg(" "+p.title+" ", "title")})
		for i, l := range p.lines {
			g.Segs(2, y+1+i, l, inner)
		}
		y += len(p.lines) + 2
	}
	g.Segs(1, y, []grid.Seg{seg(clock(s.Now)+"  ", "dim"), seg("events.jsonl keeps the log; nothing else is stored", "dim")}, width-2)
	return g
}

type panel struct {
	title string
	lines []line
}

func eventLine(e proto.Event) line {
	l := line{seg(clock(e.At)+"  ", "dim")}
	switch e.Type {
	case proto.EventMsg:
		to := e.To
		if e.Room != "" {
			to = "#" + e.Room
		}
		mark := " "
		if e.Ask {
			mark = "?"
		}
		re := ""
		if e.ReplyTo != "" {
			re = "re "
		}
		l = append(l, seg(e.From, "owner"), seg(" → ", "dim"), seg(to, "id"),
			seg(fmt.Sprintf(" (%d) ", len(e.Delivered)), "dim"), seg(mark+" ", "warn"), seg(re, "dim"), seg(oneLine(e.Body), "text"))
	case proto.EventJoined:
		l = append(l, seg(e.Member, "owner"), seg(" joined  "+e.Body, "ok"))
	case proto.EventLeft:
		l = append(l, seg(e.Member, "owner"), seg(" left  "+e.Body, "warn"))
	case proto.EventLeader:
		l = append(l, seg(e.Member, "owner"), seg(fmt.Sprintf(" leads %s (term %d, %s) %s", e.Project, e.Term, e.Action, e.Body), "title"))
	case proto.EventRoom:
		verb := " joined #"
		if e.Action == "leave" {
			verb = " left #"
		}
		l = append(l, seg(e.Member, "owner"), seg(verb+e.Room, "dim"))
	case proto.EventStatus:
		l = append(l, seg(e.Member, "owner"), seg(" is working on ", "dim"), seg(oneLine(e.Body), "text"))
	case proto.EventRefused:
		l = append(l, seg(e.From, "owner"), seg(" → "+e.To+"  refused: ", "dim"), seg(oneLine(e.Body), "bad"))
	default:
		l = append(l, seg(e.Type+" "+oneLine(e.Body), "dim"))
	}
	return l
}

// ANSIStyles maps the grid's style names to terminal colours.
func ANSIStyles() map[string]string {
	return map[string]string{
		"head":  "\x1b[1;97m",
		"frame": "\x1b[2;37m",
		"title": "\x1b[1;96m",
		"dim":   "\x1b[2;90m",
		"text":  "\x1b[0;97m",
		"ok":    "\x1b[0;92m",
		"warn":  "\x1b[0;93m",
		"bad":   "\x1b[0;91m",
		"state": "\x1b[0;94m",
		"owner": "\x1b[0;95m",
		"id":    "\x1b[0;96m",
		"spark": "\x1b[0;96m",
		"link":  "\x1b[0;94m",
		"me":    "\x1b[1;93m",
		"n1":    "\x1b[0;96m",
		"n2":    "\x1b[0;95m",
		"n3":    "\x1b[0;92m",
		"n4":    "\x1b[0;94m",
		"n5":    "\x1b[0;91m",
		"n6":    "\x1b[0;33m",
	}
}
