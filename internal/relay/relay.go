// Package relay is the louisiane message bus. It keeps who is connected, the
// projects and their leaders, and room membership, all in memory, and fans
// each message out to the members connected when it is sent. Nothing is
// stored for delivery: a short ring of recent events lets a connector that
// lost its stream for a moment catch up, and an append-only log records
// everything for people to read.
package relay

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/louisiane/internal/proto"
)

type Config struct {
	StateDir string
	// Names the relay is reached by (DNS names or IPs), for its certificate
	// and the join command.
	Names []string
	Port  int

	Grace        time.Duration // a member whose streams are all gone this long has left
	RingSize     int           // events kept for resuming streams...
	RingAge      time.Duration // ...and for at most this long
	MemoryTTL    time.Duration // message ids remembered for threading replies
	PerMinute    int           // messages per sender per minute
	AllPerMinute int           // messages to #all per sender per minute
	MaxDepth     int           // replies in one thread
	MaxBody      int           // bytes in one message
	LogMax       int64         // events.jsonl rotates at this size

	DashAuth bool
	DashUser string
	DashHash string // bcrypt; empty means dash.hash in StateDir
}

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&c.Grace, 15*time.Second)
	set(&c.RingAge, 10*time.Minute)
	set(&c.MemoryTTL, time.Hour)
	setInt := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	setInt(&c.Port, 8788)
	setInt(&c.RingSize, 2000)
	setInt(&c.PerMinute, 30)
	setInt(&c.AllPerMinute, 6)
	setInt(&c.MaxDepth, 32)
	setInt(&c.MaxBody, 32<<10)
	if c.LogMax == 0 {
		c.LogMax = 64 << 20
	}
}

type member struct {
	proto.Member
	Key       string
	droppedAt int64           // when its last stream closed; 0 while it has one
	rooms     map[string]bool // ad-hoc rooms
	watching  map[string]int  // message ids whose replies go to a CLI watch
	subs      map[*sub]bool
}

type project struct {
	Name, Key, Leader string
	Term              int
	LeaderSince       int64
}

// msgMeta is what the relay remembers of a recent message, to thread and
// route replies to it.
type msgMeta struct {
	From, Room, To, Thread string
	Depth                  int
	At                     int64
}

type roomStat struct {
	msgs int
	last int64
}

type Relay struct {
	cfg     Config
	log     *log.Logger
	Pin     string
	started time.Time
	now     func() time.Time

	mu       sync.Mutex
	epoch    string
	seq      uint64
	members  map[string]*member
	projects map[string]*project        // projects with members
	keyName  map[string]string          // project key → name, for the relay's lifetime
	nameKey  map[string]string          // project name → key
	topics   map[string]map[string]bool // ad-hoc room → members
	rstats   map[string]*roomStat
	recent   map[string]msgMeta
	order    []string // recent ids, oldest first
	ring     []*entry
	subs     map[*sub]bool
	sends    map[string][]int64
	allSends map[string][]int64
	refusals []proto.Event
	chat     []proto.Event // the dashboard's chat view
	msgTimes []int64       // message times for the throughput graph
	hostSeen map[string]int64
	invites  map[string]invite

	hashMu sync.Mutex
	hashState

	hosts  *hostStore
	events *eventLog
	kick   chan struct{} // the dashboard redraws
}

// Open loads the relay's state from cfg.StateDir.
func Open(cfg Config) (*Relay, error) {
	cfg.defaults()
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	hosts, err := loadHosts(filepath.Join(cfg.StateDir, "hosts.json"))
	if err != nil {
		return nil, err
	}
	ev, err := openEventLog(filepath.Join(cfg.StateDir, "events.jsonl"), cfg.LogMax)
	if err != nil {
		return nil, err
	}
	r := &Relay{
		cfg:      cfg,
		log:      log.New(os.Stderr, "lal relay: ", log.LstdFlags),
		started:  time.Now(),
		now:      time.Now,
		epoch:    randID("", 3)[:4],
		members:  map[string]*member{},
		projects: map[string]*project{},
		keyName:  map[string]string{},
		nameKey:  map[string]string{},
		topics:   map[string]map[string]bool{},
		rstats:   map[string]*roomStat{},
		recent:   map[string]msgMeta{},
		subs:     map[*sub]bool{},
		sends:    map[string][]int64{},
		allSends: map[string][]int64{},
		hostSeen: map[string]int64{},
		invites:  map[string]invite{},
		hosts:    hosts,
		events:   ev,
		kick:     make(chan struct{}, 1),
	}
	r.seedChat()
	return r, nil
}

func (r *Relay) Close() error { return r.events.Close() }

// SetClock replaces the relay's clock, for tests.
func (r *Relay) SetClock(now func() time.Time) { r.now = now }

func (r *Relay) ms() int64 { return r.now().UnixMilli() }

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func randID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + b32.EncodeToString(b)
}

// apiError carries an HTTP status to the handler.
type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func errf(code int, format string, args ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, args...)}
}

var nameJunk = regexp.MustCompile(`[^a-z0-9._-]+`)

// cleanName turns a directory, repo or harness name into a name part.
func cleanName(s string) string {
	s = nameJunk.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-.")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// connected reports whether m has a stream now.
func (m *member) connected() bool { return m.Streams > 0 }

// projectFor returns the project name for key, registering it under want
// (or want@host when another key holds want) the first time.
func (r *Relay) projectFor(key, want, host string) (name, note string, err error) {
	if n, ok := r.keyName[key]; ok {
		if n != want && !strings.HasPrefix(n, want+"@") {
			note = fmt.Sprintf("This project is on the mesh as %s.", n)
		}
		return n, note, nil
	}
	name = want
	if want == Owner {
		r.nameKey[Owner] = "reserved: the owner's address"
	}
	if other, ok := r.nameKey[name]; ok {
		name = want + "@" + cleanName(host)
		note = fmt.Sprintf("Another project (%s) is already called %s, so this one is %s. Choose a name with `lal prime --as NAME` in AGENTS.md.", other, want, name)
		if _, ok := r.nameKey[name]; ok {
			return "", "", errf(409, "project names %s and %s are both taken; choose one with `lal prime --as NAME`", want, name)
		}
	}
	if _, ok := r.topics[name]; ok {
		return "", "", errf(409, "#%s is an ad-hoc room; choose a project name with `lal prime --as NAME`", name)
	}
	r.keyName[key], r.nameKey[name] = name, key
	return name, note, nil
}

// projectMembers lists p's members, most senior first.
func (r *Relay) projectMembers(p string) []*member {
	var out []*member
	for _, m := range r.members {
		if m.Project == p {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt != out[j].ConnectedAt {
			return out[i].ConnectedAt < out[j].ConnectedAt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func names(ms []*member) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// setLeader makes next lead p and tells the project. by is who caused it.
func (r *Relay) setLeader(p *project, next, action, by string) {
	prev := p.Leader
	p.Leader, p.LeaderSince = next, r.ms()
	p.Term++
	r.log.Printf("%s: %s leads (term %d, %s)", p.Name, next, p.Term, action)
	ev := proto.Event{Type: proto.EventLeader, Project: p.Name, Member: next, Term: p.Term, Action: action, From: by}
	if prev != "" {
		ev.Body = "after " + prev
	}
	r.publish(ev, names(r.projectMembers(p.Name)))
}

// elect gives p a leader when it has none: its most senior member.
func (r *Relay) elect(p *project) {
	if l, ok := r.members[p.Leader]; ok && l.Project == p.Name {
		return
	}
	ms := r.projectMembers(p.Name)
	if len(ms) == 0 {
		p.Leader = ""
		return
	}
	r.setLeader(p, ms[0].Name, "elected", "relay")
}

// remove takes a member off the mesh.
func (r *Relay) remove(m *member, why string) {
	delete(r.members, m.Name)
	for room := range m.rooms {
		delete(r.topics[room], m.Name)
		if len(r.topics[room]) == 0 {
			delete(r.topics, room)
		}
	}
	for s := range m.subs {
		s.close()
	}
	r.publish(proto.Event{Type: proto.EventLeft, Member: m.Name, Project: m.Project, Body: why}, nil)
	if p := r.projects[m.Project]; p != nil {
		if len(r.projectMembers(p.Name)) == 0 {
			delete(r.projects, p.Name)
		} else if p.Leader == m.Name {
			p.Leader = ""
			r.elect(p)
		}
	}
}

// Sweep drops members whose streams have been gone longer than the grace,
// and forgets old message ids and rate counts.
func (r *Relay) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.ms()
	for _, m := range r.members {
		if m.Streams == 0 && now-m.droppedAt > r.cfg.Grace.Milliseconds() {
			r.remove(m, "disconnected")
		}
	}
	cut := now - r.cfg.MemoryTTL.Milliseconds()
	i := 0
	for ; i < len(r.order); i++ {
		if r.recent[r.order[i]].At >= cut {
			break
		}
		delete(r.recent, r.order[i])
	}
	r.order = r.order[i:]
	minute := now - time.Minute.Milliseconds()
	for _, rates := range []map[string][]int64{r.sends, r.allSends} {
		for k, ts := range rates {
			ts = slices.DeleteFunc(ts, func(t int64) bool { return t < minute })
			if len(ts) == 0 {
				delete(rates, k)
			} else {
				rates[k] = ts
			}
		}
	}
	graph := now - 5*time.Minute.Milliseconds()
	r.msgTimes = slices.DeleteFunc(r.msgTimes, func(t int64) bool { return t < graph })
}

// Run sweeps once a second until done closes.
func (r *Relay) Run(done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			r.Sweep()
			r.redraw()
		}
	}
}

func (r *Relay) redraw() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// view renders a member for the wire.
func (r *Relay) view(m *member) proto.Member {
	v := m.Member
	v.Rooms = nil
	for room := range m.rooms {
		v.Rooms = append(v.Rooms, room)
	}
	sort.Strings(v.Rooms)
	if p := r.projects[m.Project]; p != nil {
		v.Leader = p.Leader == m.Name
	}
	return v
}

func (r *Relay) projectView(p *project) proto.Project {
	v := proto.Project{Name: p.Name, Key: p.Key, Leader: p.Leader, Term: p.Term, LeaderSince: p.LeaderSince, Members: []proto.Member{}}
	for _, m := range r.projectMembers(p.Name) {
		v.Members = append(v.Members, r.view(m))
	}
	return v
}

func (r *Relay) who() proto.Who {
	w := proto.Who{Projects: []proto.Project{}, Rooms: []proto.Room{}}
	var ps []string
	for n := range r.projects {
		ps = append(ps, n)
	}
	sort.Strings(ps)
	all := []string{}
	for _, n := range ps {
		v := r.projectView(r.projects[n])
		w.Projects = append(w.Projects, v)
		ms := []string{}
		for _, m := range v.Members {
			ms = append(ms, m.Name)
			all = append(all, m.Name)
		}
		w.Rooms = append(w.Rooms, r.roomView(n, "project", ms))
	}
	w.Rooms = append([]proto.Room{r.roomView(proto.AllRoom, "all", all)}, w.Rooms...)
	var ts []string
	for t := range r.topics {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	for _, t := range ts {
		ms := []string{}
		for m := range r.topics[t] {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		w.Rooms = append(w.Rooms, r.roomView(t, "topic", ms))
	}
	w.Hosts = r.hosts.names()
	return w
}

func (r *Relay) roomView(name, kind string, ms []string) proto.Room {
	rm := proto.Room{Name: name, Kind: kind, Members: ms}
	if st := r.rstats[name]; st != nil {
		rm.Msgs, rm.LastAt = st.msgs, st.last
	}
	return rm
}
