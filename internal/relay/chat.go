package relay

import (
	"net/http"
	"sort"
	"time"

	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/proto"
)

// The chat view's buffer: the recent conversation, for the owner to read on
// the dashboard. Like the log it is for people; nothing is delivered from
// it. It is seeded from the log at start, so a restart doesn't blank it.
const chatKeep = 2000

func chatType(t string) bool {
	switch t {
	case proto.EventMsg, proto.EventJoined, proto.EventLeft, proto.EventLeader, proto.EventRoom, proto.EventStatus, proto.EventRefused:
		return true
	}
	return false
}

// keepChat adds ev to the chat buffer. Called with r.mu held.
func (r *Relay) keepChat(ev proto.Event) {
	if !chatType(ev.Type) {
		return
	}
	r.chat = append(r.chat, ev)
	if len(r.chat) > chatKeep+chatKeep/4 {
		r.chat = append(r.chat[:0:0], r.chat[len(r.chat)-chatKeep:]...)
	}
}

func (r *Relay) seedChat() {
	evs, err := r.readLog(LogQuery{Since: time.Now().Add(-48 * time.Hour), Limit: chatKeep, All: true})
	if err != nil {
		r.log.Printf("chat history: %v", err)
		return
	}
	r.chat = evs
}

// inChannel reports whether ev belongs in the chat view's channel room:
// "*" (everything), "direct" (DMs), or a room.
func inChannel(ev proto.Event, room string) bool {
	switch room {
	case "*":
		return true
	case "direct":
		return ev.Type == proto.EventMsg && ev.Room == ""
	}
	switch ev.Type {
	case proto.EventMsg:
		return ev.Room == room
	case proto.EventJoined, proto.EventLeft:
		return room == proto.AllRoom || ev.Project == room
	case proto.EventLeader, proto.EventStatus:
		return ev.Project == room
	case proto.EventRoom:
		return ev.Room == room
	case proto.EventRefused:
		return ev.To == "#"+room
	}
	return false
}

// Chat is the chat view of one channel: its newest n events.
func (r *Relay) Chat(room string, n int) dash.ChatView {
	if room == "" {
		room = "*"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := dash.ChatView{Room: room, Epoch: r.epoch, Now: r.ms(), CanSend: r.cfg.DashAuth, Agents: len(r.members)}
	if len(r.cfg.Names) > 0 {
		v.Relay = r.cfg.Names[0]
	}
	counts := map[string]int{}
	for _, ev := range r.chat {
		if ev.Type != proto.EventMsg {
			continue
		}
		counts["*"]++
		if ev.Room == "" {
			counts["direct"]++
		} else {
			counts[ev.Room]++
		}
	}
	for i := len(r.chat) - 1; i >= 0 && len(v.Events) < n; i-- {
		if inChannel(r.chat[i], room) {
			v.Events = append(v.Events, r.chat[i])
		}
	}
	for i, j := 0, len(v.Events)-1; i < j; i, j = i+1, j-1 {
		v.Events[i], v.Events[j] = v.Events[j], v.Events[i]
	}

	// Channels: everything, #all, the projects' rooms, the ad-hoc rooms,
	// rooms with recent talk but nobody left in them, and the DMs.
	v.Channels = append(v.Channels, dash.Channel{Name: "*", Label: "*", Msgs: counts["*"]},
		dash.Channel{Name: proto.AllRoom, Label: "#all", Msgs: counts[proto.AllRoom], Members: len(r.members)})
	seen := map[string]bool{"*": true, proto.AllRoom: true, "direct": true}
	var rest []dash.Channel
	for name := range r.projects {
		seen[name] = true
		rest = append(rest, dash.Channel{Name: name, Label: "#" + name, Msgs: counts[name], Members: len(r.projectMembers(name)), Kind: "project"})
	}
	for name, ms := range r.topics {
		seen[name] = true
		rest = append(rest, dash.Channel{Name: name, Label: "#" + name, Msgs: counts[name], Members: len(ms), Kind: "topic"})
	}
	for name, c := range counts {
		if !seen[name] {
			rest = append(rest, dash.Channel{Name: name, Label: "#" + name, Msgs: c, Kind: "quiet"})
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	v.Channels = append(v.Channels, rest...)
	v.Channels = append(v.Channels, dash.Channel{Name: "direct", Label: "@direct", Msgs: counts["direct"]})
	return v
}

func (r *Relay) handleChat(w http.ResponseWriter, req *http.Request) error {
	if err := r.reader(req); err != nil {
		return err
	}
	writeJSON(w, 200, r.Chat(req.URL.Query().Get("room"), 200))
	return nil
}
