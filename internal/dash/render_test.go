package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/louisiane/internal/grid"
	"github.com/jrimmer/louisiane/internal/proto"
)

func sample() Snapshot {
	now := time.Now().UnixMilli()
	return Snapshot{
		Relay: "rack.example.lan", Epoch: "k3f9", Started: now - 3_600_000, Now: now, Seq: 42, Bucket: 5,
		Hosts: []HostSeen{{Name: "desk"}},
		Projects: []proto.Project{{Name: "billing", Key: "git.example.lan/team/billing", Leader: "billing/omp-91c0", Term: 2,
			Members: []proto.Member{
				{Name: "billing/omp-91c0", Kind: "omp", Host: "desk", Leader: true, Streams: 1, ConnectedAt: now - 60_000, WorkingOn: "lib/api.ex"},
				{Name: "billing/claude-3f2a", Kind: "claude", Host: "desk", ConnectedAt: now - 30_000},
			}}},
		Rooms:    []proto.Room{{Name: "all", Kind: "all", Members: []string{"a", "b"}, Msgs: 3, LastAt: now}},
		Events:   []proto.Event{{Type: proto.EventMsg, At: now, From: "api/pi-1", Room: "all", Ask: true, Body: "port 4000?", Delivered: []string{"a"}}},
		Refusals: []proto.Event{{Type: proto.EventRefused, At: now, From: "x/y", To: "#nope", Body: "nobody is in #nope"}},
		Rate:     make([]float64, 60),
	}
}

func TestRenderUsesOnlyFontGlyphs(t *testing.T) {
	for _, w := range []int{MinWidth, DefaultWidth, 200} {
		g := Render(sample(), w)
		if g.Cols() != max(MinWidth, min(w, DefaultWidth)) {
			t.Fatalf("width %d drew %d", w, g.Cols())
		}
		var extra strings.Builder
		for _, r := range g.Plain() {
			if r > 0x7e && grid.FontHas(r) {
				extra.WriteRune(r)
			}
		}
		if err := g.Check(extra.String()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(g.Plain(), "◆ omp-91c0") {
			t.Fatalf("leader not marked:\n%s", g.Plain())
		}
	}
}

func TestRenderChat(t *testing.T) {
	now := time.Now().UnixMilli()
	v := ChatView{Relay: "rack", Epoch: "k3f9", Now: now, Room: "*", CanSend: true, Agents: 3,
		Channels: []Channel{{Name: "*", Label: "*", Msgs: 3}, {Name: "all", Label: "#all", Msgs: 1}, {Name: "bez", Label: "#bez", Kind: "project"}, {Name: "direct", Label: "@direct", Msgs: 2}},
		Events: []proto.Event{
			{Type: proto.EventJoined, At: now, Member: "bez/omp-1", Body: "omp on desk"},
			{Type: proto.EventMsg, At: now, ID: "m_aaaa", From: "api/pi-1", Room: "all", Ask: true, Body: strings.Repeat("Can I take the build box down for fifteen minutes? ", 5)},
			{Type: proto.EventMsg, At: now, ID: "m_bbbb", From: "bez/omp-1", To: "api/pi-1", ReplyTo: "m_aaaa", Body: "yes\nbut after 5"},
			{Type: proto.EventMsg, At: now, ID: "m_cccc", From: "owner", Room: "all", Body: "Freeze merges."},
		}}
	for _, w := range []int{MinWidth, DefaultWidth} {
		g := RenderChat(v, w)
		var extra strings.Builder
		for _, r := range g.Plain() {
			if r > 0x7e && grid.FontHas(r) {
				extra.WriteRune(r)
			}
		}
		if err := g.Check(extra.String()); err != nil {
			t.Fatal(err)
		}
		plain := g.Plain()
		for _, want := range []string{"[chat]", "[* 3]", "-!- bez/omp-1 joined", "? Can I take", "↪ yes", "m_bbbb", "Freeze merges."} {
			if !strings.Contains(plain, want) {
				t.Fatalf("width %d: missing %q in\n%s", w, want, plain)
			}
		}
		if !strings.Contains(g.HTML(), `data-id="reply:m_aaaa"`) || !strings.Contains(g.HTML(), `data-id="go:/chat?room=all"`) {
			t.Fatal("ids and channels are not clickable")
		}
	}
}
