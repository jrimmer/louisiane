package relay

import (
	"sort"

	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/proto"
)

const (
	rateBuckets = 60
	rateBucket  = 5 // seconds
)

// Snapshot is the dashboard's view of the relay now.
func (r *Relay) Snapshot() dash.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.ms()
	w := r.who()
	s := dash.Snapshot{Epoch: r.epoch, Started: r.started.UnixMilli(), Now: now, Seq: r.seq,
		Projects: w.Projects, Rooms: w.Rooms, Bucket: rateBucket}
	if len(r.cfg.Names) > 0 {
		s.Relay = r.cfg.Names[0]
	}
	if st := r.rstats["dm"]; st != nil {
		s.Rooms = append(s.Rooms, proto.Room{Name: "dm", Msgs: st.msgs, LastAt: st.last, Members: []string{}})
	}
	counts := map[string]int{}
	for _, m := range r.members {
		counts[m.Host]++
	}
	for _, h := range r.hosts.list() {
		s.Hosts = append(s.Hosts, dash.HostSeen{Name: h.Name, LastSeen: r.hostSeen[h.Name], Members: counts[h.Name]})
	}
	sort.Slice(s.Hosts, func(i, j int) bool { return s.Hosts[i].Name < s.Hosts[j].Name })
	for i := len(r.ring) - 1; i >= 0 && len(s.Events) < 40; i-- {
		switch r.ring[i].ev.Type {
		case proto.EventMsg, proto.EventJoined, proto.EventLeft, proto.EventLeader, proto.EventRoom, proto.EventStatus, proto.EventRefused:
			s.Events = append(s.Events, r.ring[i].ev)
		}
	}
	for i, j := 0, len(s.Events)-1; i < j; i, j = i+1, j-1 {
		s.Events[i], s.Events[j] = s.Events[j], s.Events[i]
	}
	s.Refusals = append(s.Refusals, r.refusals...)
	if len(s.Refusals) > 8 {
		s.Refusals = s.Refusals[len(s.Refusals)-8:]
	}
	s.Rate = make([]float64, rateBuckets)
	start := now - rateBuckets*rateBucket*1000
	for _, t := range r.msgTimes {
		if t < start {
			continue
		}
		b := int((t - start) / (rateBucket * 1000))
		if b >= 0 && b < rateBuckets {
			s.Rate[b]++
		}
		if now-t < 60_000 {
			s.PerMin++
		}
	}
	return s
}
