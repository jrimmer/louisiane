package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jrimmer/louisiane/internal/proto"
)

// entry is one event in the ring, with the members it is for.
type entry struct {
	ev proto.Event
	to map[string]bool
}

// sub is one open stream: a connector's (member set), a CLI watch on the
// replies to one message (member and replyTo set), or a tail of everything.
type sub struct {
	member  string
	replyTo string
	all     bool
	ch      chan *entry
	done    chan struct{}
	once    sync.Once
	lost    bool // events were dropped because the reader fell behind
}

func newSub() *sub {
	return &sub{ch: make(chan *entry, 512), done: make(chan struct{})}
}

func (s *sub) close() { s.once.Do(func() { close(s.done) }) }

// wants reports whether s should carry e. Called with r.mu held.
func (r *Relay) wants(s *sub, e *entry) bool {
	switch {
	case s.all:
		return true
	case s.replyTo != "":
		return e.ev.Type == proto.EventMsg && e.ev.ReplyTo == s.replyTo && e.to[s.member]
	}
	if !e.to[s.member] {
		return false
	}
	if e.ev.Type == proto.EventMsg && e.ev.ReplyTo != "" {
		if m := r.members[s.member]; m != nil && m.watching[e.ev.ReplyTo] > 0 {
			return false // a CLI watch is collecting these
		}
	}
	return true
}

// publish stamps ev, keeps it in the ring, logs it and hands it to every
// stream that wants it. to lists the members it is for. Called with r.mu
// held; it never blocks on a slow reader.
func (r *Relay) publish(ev proto.Event, to []string) proto.Event {
	r.seq++
	ev.Seq, ev.Epoch, ev.At = r.seq, r.epoch, r.ms()
	e := &entry{ev: ev, to: map[string]bool{}}
	for _, n := range to {
		e.to[n] = true
	}
	r.ring = append(r.ring, e)
	cut := ev.At - r.cfg.RingAge.Milliseconds()
	drop := 0
	for drop < len(r.ring) && (len(r.ring)-drop > r.cfg.RingSize || r.ring[drop].ev.At < cut) {
		drop++
	}
	if drop > 0 {
		r.ring = append(r.ring[:0:0], r.ring[drop:]...)
	}
	r.events.write(ev)
	r.keepChat(ev)
	for s := range r.subs {
		if !r.wants(s, e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.lost = true
		}
	}
	r.redraw()
	return ev
}

// subscribe registers s and returns what it missed since lastID, and
// whether there is a hole it cannot fill. Called with r.mu held.
func (r *Relay) subscribe(s *sub, lastID string) (missed []*entry, gap bool) {
	r.subs[s] = true
	if lastID == "" {
		return nil, false
	}
	epoch, seq, ok := parseEventID(lastID)
	if !ok || epoch != r.epoch {
		return nil, true
	}
	if len(r.ring) > 0 && r.ring[0].ev.Seq > seq+1 {
		gap = true
	}
	for _, e := range r.ring {
		if e.ev.Seq > seq && r.wants(s, e) {
			missed = append(missed, e)
		}
	}
	return missed, gap
}

func (r *Relay) unsubscribe(s *sub) {
	delete(r.subs, s)
	s.close()
}

// EventID is an event's SSE id: "<epoch>-<seq>". A new epoch after a
// restart tells a resuming stream its position is gone.
func EventID(ev proto.Event) string { return ev.Epoch + "-" + strconv.FormatUint(ev.Seq, 10) }

func parseEventID(id string) (string, uint64, bool) {
	epoch, s, ok := strings.Cut(id, "-")
	if !ok || epoch == "" {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	return epoch, seq, err == nil
}

// eventLog appends every event to events.jsonl, rotating by size. It
// writes from its own goroutine so the bus never waits on the disk.
type eventLog struct {
	path string
	max  int64
	ch   chan []byte
	done chan struct{}
}

func openEventLog(path string, max int64) (*eventLog, error) {
	l := &eventLog{path: path, max: max, ch: make(chan []byte, 4096), done: make(chan struct{})}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	go l.run(f)
	return l, nil
}

func (l *eventLog) write(ev proto.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	select {
	case l.ch <- append(b, '\n'):
	default: // the disk is behind; the log is for people, so drop rather than block
	}
}

func (l *eventLog) run(f *os.File) {
	defer close(l.done)
	w := bufio.NewWriter(f)
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	for b := range l.ch {
		n, _ := w.Write(b)
		size += int64(n)
		if len(l.ch) == 0 {
			w.Flush()
		}
		if size >= l.max {
			w.Flush()
			f.Close()
			l.rotate()
			var err error
			if f, err = os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "lal relay: event log: %v\n", err)
				for range l.ch {
				}
				return
			}
			w.Reset(f)
			size = 0
		}
	}
	w.Flush()
	f.Close()
}

func (l *eventLog) rotate() {
	for i := 2; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	os.Rename(l.path, l.path+".1")
}

func (l *eventLog) Close() error {
	close(l.ch)
	<-l.done
	return nil
}
