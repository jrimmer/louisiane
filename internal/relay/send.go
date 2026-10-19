package relay

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/proto"
)

// ConnectReq is what a connector says about itself when its stream opens.
type ConnectReq struct {
	Key, Name, Root, Kind string
	PID                   int
	Member                string   // the name it had before, to keep across a reconnect
	Rooms                 []string // ad-hoc rooms to rejoin
}

// connect attaches a stream to a member, making the member if it is new
// (or was lost to a relay restart). Called with r.mu held.
func (r *Relay) connect(host string, req ConnectReq, s *sub) (*member, *proto.Hello, error) {
	now := r.ms()
	if m := r.members[req.Member]; m != nil && req.Member != "" {
		if m.Host != host {
			return nil, nil, errf(403, "%s belongs to another host", req.Member)
		}
		m.Streams++
		m.droppedAt, m.LastSeen = 0, now
		m.subs[s] = true
		s.member = m.Name
		return m, r.hello(m, "", true), nil
	}
	if req.Key == "" {
		return nil, nil, errf(400, "connect needs a project key")
	}
	want := cleanName(req.Name)
	if want == "" || want == proto.AllRoom {
		return nil, nil, errf(400, "project name %q is not usable; choose one with `lal prime --as NAME`", req.Name)
	}
	pname, note, err := r.projectFor(req.Key, want, host)
	if err != nil {
		return nil, nil, err
	}
	kind := cleanName(req.Kind)
	if kind == "" {
		kind = "agent"
	}
	name := req.Member
	if name == "" || !strings.HasPrefix(name, pname+"/") || r.members[name] != nil {
		for {
			name = pname + "/" + kind + "-" + randID("", 3)[:4]
			if r.members[name] == nil {
				break
			}
		}
	}
	m := &member{
		Member: proto.Member{Name: name, Project: pname, Host: host, Kind: kind, PID: req.PID, Root: req.Root,
			ConnectedAt: now, LastSeen: now, Streams: 1},
		Key:      req.Key,
		rooms:    map[string]bool{},
		watching: map[string]int{},
		subs:     map[*sub]bool{s: true},
	}
	s.member = name
	r.members[name] = m
	p := r.projects[pname]
	if p == nil {
		p = &project{Name: pname, Key: req.Key}
		r.projects[pname] = p
	}
	r.publish(proto.Event{Type: proto.EventJoined, Member: name, Project: pname, Body: kind + " on " + host}, nil)
	for _, room := range req.Rooms {
		if n, err := r.topicName(room); err == nil {
			r.joinRoom(m, n)
		}
	}
	r.elect(p)
	return m, r.hello(m, note, false), nil
}

func (r *Relay) hello(m *member, note string, resumed bool) *proto.Hello {
	h := &proto.Hello{Member: m.Name, Project: r.projectView(r.projects[m.Project]), Note: note, Resumed: resumed,
		Rooms: []string{proto.AllRoom, m.Project}}
	for room := range m.rooms {
		h.Rooms = append(h.Rooms, room)
	}
	sort.Strings(h.Rooms[2:])
	return h
}

// detach is called when one of m's streams closes.
func (r *Relay) detach(m *member, s *sub) {
	delete(m.subs, s)
	if m.Streams > 0 {
		m.Streams--
	}
	if m.Streams == 0 {
		m.droppedAt = r.ms()
	}
}

// topicName validates an ad-hoc room name.
func (r *Relay) topicName(s string) (string, error) {
	n := cleanName(strings.TrimPrefix(s, "#"))
	switch {
	case n == "" || n != strings.ToLower(strings.TrimPrefix(s, "#")):
		return "", errf(400, "bad room name %q (use a-z, 0-9, '.', '_' and '-')", s)
	case n == proto.AllRoom:
		return "", errf(400, "everyone is always in #all")
	case r.nameKey[n] != "":
		return "", errf(400, "#%s is project %s's room; its members are in it automatically", n, n)
	}
	return n, nil
}

func (r *Relay) joinRoom(m *member, room string) {
	if m.rooms[room] {
		return
	}
	m.rooms[room] = true
	if r.topics[room] == nil {
		r.topics[room] = map[string]bool{}
	}
	r.topics[room][m.Name] = true
	r.publish(proto.Event{Type: proto.EventRoom, Member: m.Name, Room: room, Action: "join"}, []string{m.Name})
}

func (r *Relay) leaveRoom(m *member, room string) {
	if !m.rooms[room] {
		return
	}
	delete(m.rooms, room)
	delete(r.topics[room], m.Name)
	if len(r.topics[room]) == 0 {
		delete(r.topics, room)
	}
	r.publish(proto.Event{Type: proto.EventRoom, Member: m.Name, Room: room, Action: "leave"}, []string{m.Name})
}

// Room joins or leaves an ad-hoc room.
func (r *Relay) Room(name, room string, join bool) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[name]
	if m == nil {
		return nil, notConnected(name)
	}
	n, err := r.topicName(room)
	if err != nil {
		return nil, err
	}
	if join {
		r.joinRoom(m, n)
	} else {
		r.leaveRoom(m, n)
	}
	ms := []string{}
	for x := range r.topics[n] {
		ms = append(ms, x)
	}
	sort.Strings(ms)
	return ms, nil
}

func notConnected(name string) error {
	return errf(409, "%s is not connected to the mesh; run `lal prime` to connect this session", name)
}

// resolve turns an address into the members it reaches (without the
// sender) and how the message records it: a room or a DM address.
func (r *Relay) resolve(sender, to string) (room, dm string, rcpt []string, err error) {
	to = strings.TrimSpace(to)
	if rest, ok := strings.CutPrefix(to, "#"); ok {
		room = strings.ToLower(rest)
		switch {
		case room == proto.AllRoom:
			for n := range r.members {
				rcpt = append(rcpt, n)
			}
		case r.projects[room] != nil:
			rcpt = names(r.projectMembers(room))
		case len(r.topics[room]) > 0:
			for n := range r.topics[room] {
				rcpt = append(rcpt, n)
			}
		default:
			return "", "", nil, errf(404, "nobody is in #%s; `lal who` lists the rooms", room)
		}
	} else {
		dm = strings.TrimPrefix(to, "@")
		pname, rest, hasMember := strings.Cut(dm, "/")
		switch {
		case dm == Owner:
			// The owner reads replies on the dashboard and in the log.
			rcpt = []string{Owner}
		case hasMember && rest != "leader":
			if r.members[dm] == nil {
				return "", "", nil, errf(404, "%s is not connected; `lal who %s` lists who is", dm, pname)
			}
			rcpt = []string{dm}
		default:
			p := r.projects[pname]
			if p == nil {
				return "", "", nil, errf(404, "nobody from %s is connected; `lal who` lists the projects", pname)
			}
			if p.Leader == sender {
				return "", "", nil, errf(400, "you lead %s: post in #%s, or DM a member", pname, pname)
			}
			if p.Leader == "" {
				return "", "", nil, errf(409, "%s has no leader right now", pname)
			}
			rcpt = []string{p.Leader}
		}
	}
	rcpt = slices.DeleteFunc(rcpt, func(n string) bool { return n == sender })
	sort.Strings(rcpt)
	return room, dm, rcpt, nil
}

// rate counts a send against a limit of n a minute.
func (r *Relay) rate(rates map[string][]int64, who string, n int, what string) error {
	now := r.ms()
	minute := now - time.Minute.Milliseconds()
	ts := slices.DeleteFunc(rates[who], func(t int64) bool { return t < minute })
	if len(ts) >= n {
		rates[who] = ts
		return errf(429, "%s limit reached (%d a minute); slow down", what, n)
	}
	rates[who] = append(ts, now)
	return nil
}

// Send delivers a message from sender (a member name, or "@host" for a
// host script) to whoever its address reaches now.
func (r *Relay) Send(sender string, req proto.SendReq) (*proto.SendResp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resp, err := r.send(sender, req)
	if err != nil {
		reason := err.Error()
		r.refusals = append(r.refusals, r.publish(proto.Event{Type: proto.EventRefused, From: sender, To: req.To,
			ReplyTo: req.ReplyTo, Body: reason}, nil))
		if len(r.refusals) > 50 {
			r.refusals = r.refusals[len(r.refusals)-50:]
		}
	}
	return resp, err
}

func (r *Relay) send(sender string, req proto.SendReq) (*proto.SendResp, error) {
	if m := r.members[sender]; m == nil && !strings.HasPrefix(sender, "@") && sender != Owner {
		return nil, notConnected(sender)
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, errf(400, "empty message")
	}
	if len(req.Body) > r.cfg.MaxBody {
		return nil, errf(400, "messages are limited to %d KB; everything sent ends up in someone's context", r.cfg.MaxBody>>10)
	}
	parent, known := r.recent[req.ReplyTo]
	to := req.To
	if to == "" {
		switch {
		case req.ReplyTo == "":
			return nil, errf(400, "no address")
		case !known:
			return nil, errf(404, "%s is not in the relay's memory of the last %s; address the reply yourself", req.ReplyTo, r.cfg.MemoryTTL)
		case req.InRoom && parent.Room != "":
			to = "#" + parent.Room
		case strings.HasPrefix(parent.From, "@"):
			return nil, errf(400, "%s came from a script on %s; reply in #%s with --room, or to someone connected", req.ReplyTo, parent.From[1:], parent.Room)
		default:
			to = parent.From
		}
	}
	room, dm, rcpt, err := r.resolve(sender, to)
	if err != nil {
		return nil, err
	}
	// A reply reaches the author of what it answers, wherever it is posted.
	if known && parent.From != sender && (r.members[parent.From] != nil || parent.From == Owner) && !slices.Contains(rcpt, parent.From) {
		rcpt = append(rcpt, parent.From)
		sort.Strings(rcpt)
	}
	if len(rcpt) == 0 {
		return nil, errf(409, "nobody else is in %s right now", to)
	}
	thread, depth := "", 0
	switch {
	case known:
		thread, depth = parent.Thread, parent.Depth+1
	case req.ReplyTo != "":
		thread, depth = req.ReplyTo, 1
	}
	if depth > r.cfg.MaxDepth {
		return nil, errf(429, "this thread is %d messages deep (limit %d); if it matters, start a new message, otherwise let it rest", depth-1, r.cfg.MaxDepth)
	}
	if err := r.rate(r.sends, sender, r.cfg.PerMinute, "message"); err != nil {
		return nil, err
	}
	if room == proto.AllRoom {
		if err := r.rate(r.allSends, sender, r.cfg.AllPerMinute, "#all"); err != nil {
			return nil, err
		}
	}
	id := randID("m_", 4)
	if thread == "" {
		thread = id
	}
	ev := r.publish(proto.Event{Type: proto.EventMsg, ID: id, From: sender, To: dm, Room: room, ReplyTo: req.ReplyTo,
		Thread: thread, Depth: depth, Ask: req.Ask, Body: req.Body, Delivered: rcpt}, rcpt)
	r.recent[id] = msgMeta{From: sender, Room: room, To: dm, Thread: thread, Depth: depth, At: ev.At}
	r.order = append(r.order, id)
	r.msgTimes = append(r.msgTimes, ev.At)
	if m := r.members[sender]; m != nil {
		m.Sent++
		m.LastSeen = ev.At
	}
	for _, n := range rcpt {
		if m := r.members[n]; m != nil {
			m.Received++
		}
	}
	key := room
	if key == "" {
		key = "dm"
	}
	st := r.rstats[key]
	if st == nil {
		st = &roomStat{}
		r.rstats[key] = st
	}
	st.msgs++
	st.last = ev.At
	return &proto.SendResp{ID: id, Seq: ev.Seq, To: to, Delivered: rcpt}, nil
}

// Lead hands off or takes a project's lead.
func (r *Relay) Lead(name string, req proto.LeadReq) (*proto.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[name]
	if m == nil {
		return nil, notConnected(name)
	}
	p := r.projects[m.Project]
	switch req.Action {
	case "handoff":
		if p.Leader != name {
			return nil, errf(403, "only the leader (%s) can hand off", p.Leader)
		}
		to := req.To
		if !strings.Contains(to, "/") {
			to = p.Name + "/" + to
		}
		next := r.members[to]
		if next == nil || next.Project != p.Name {
			return nil, errf(404, "%s is not a connected member of %s", to, p.Name)
		}
		if next.Name == name {
			return nil, errf(400, "you already lead %s", p.Name)
		}
		r.setLeader(p, next.Name, "handoff", name)
	case "take":
		if p.Leader == name {
			return nil, errf(400, "you already lead %s", p.Name)
		}
		r.setLeader(p, name, "take", name)
	default:
		return nil, errf(400, "lead action must be take or handoff")
	}
	v := r.projectView(p)
	return &v, nil
}

// Status sets a member's working-on line.
func (r *Relay) Status(name, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[name]
	if m == nil {
		return notConnected(name)
	}
	if len(text) > 200 {
		return errf(400, "status is limited to 200 bytes")
	}
	m.WorkingOn = text
	r.publish(proto.Event{Type: proto.EventStatus, Member: name, Project: m.Project, Body: text}, nil)
	return nil
}

// Who is the roster.
func (r *Relay) Who() proto.Who {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.who()
}

func (r *Relay) touch(name string) {
	if m := r.members[name]; m != nil {
		m.LastSeen = r.ms()
	}
}
