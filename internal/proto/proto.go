// Package proto holds the JSON types shared by the relay and its clients.
// Times are Unix milliseconds.
package proto

// Event types.
const (
	EventHello   = "hello"   // first event on a connector's stream, to it alone
	EventMsg     = "msg"     // a message
	EventJoined  = "joined"  // a member connected
	EventLeft    = "left"    // a member left
	EventLeader  = "leader"  // a project has a new leader
	EventRoom    = "room"    // a member joined or left an ad-hoc room
	EventStatus  = "status"  // a member's working-on line changed
	EventGap     = "gap"     // the stream lost events
	EventRefused = "refused" // a send that reached nobody or hit a limit (log and dashboard)
)

// AllRoom is the room every connected member is in.
const AllRoom = "all"

// Event is one thing that happened on the bus.
type Event struct {
	Seq     uint64 `json:"seq"`
	Epoch   string `json:"epoch"`
	At      int64  `json:"at"`
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"` // message id
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`   // a DM's address, as written
	Room    string `json:"room,omitempty"` // a room message's room, without '#'
	ReplyTo string `json:"reply_to,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Depth   int    `json:"depth,omitempty"`
	Ask     bool   `json:"ask,omitempty"`
	Body    string `json:"body,omitempty"`

	// Membership, leadership and status events.
	Member  string `json:"member,omitempty"`
	Project string `json:"project,omitempty"`
	Term    int    `json:"term,omitempty"`
	Action  string `json:"action,omitempty"` // room: join or leave; leader: elected, handoff or take

	// Delivered lists the members a message reached.
	Delivered []string `json:"delivered,omitempty"`

	// Hello carries the new member's view of the mesh.
	Hello *Hello `json:"hello,omitempty"`
}

// Hello is what a connector learns when its stream opens.
type Hello struct {
	Member  string   `json:"member"`
	Project Project  `json:"project"`
	Rooms   []string `json:"rooms"`
	Note    string   `json:"note,omitempty"`
	Resumed bool     `json:"resumed,omitempty"`
}

type Member struct {
	Name        string   `json:"name"`
	Project     string   `json:"project"`
	Host        string   `json:"host"`
	Kind        string   `json:"kind"`
	PID         int      `json:"pid,omitempty"`
	Root        string   `json:"root,omitempty"`
	WorkingOn   string   `json:"working_on,omitempty"`
	ConnectedAt int64    `json:"connected_at"`
	LastSeen    int64    `json:"last_seen"`
	Leader      bool     `json:"leader,omitempty"`
	Streams     int      `json:"streams"`
	Rooms       []string `json:"rooms,omitempty"` // ad-hoc rooms
	Sent        int      `json:"sent"`
	Received    int      `json:"received"`
}

type Project struct {
	Name        string   `json:"name"`
	Key         string   `json:"key"`
	Leader      string   `json:"leader,omitempty"`
	Term        int      `json:"term"`
	LeaderSince int64    `json:"leader_since,omitempty"`
	Members     []Member `json:"members"`
}

type Room struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // all, project or topic
	Members []string `json:"members"`
	Msgs    int      `json:"msgs"` // messages since the relay started
	LastAt  int64    `json:"last_at,omitempty"`
}

type Who struct {
	Projects []Project `json:"projects"`
	Rooms    []Room    `json:"rooms"`
	Hosts    []string  `json:"hosts,omitempty"`
}

type JoinReq struct {
	Code string `json:"code"`
}

type JoinResp struct {
	Host  string `json:"host"`
	Token string `json:"token"`
}

type SendReq struct {
	To      string `json:"to"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
	Ask     bool   `json:"ask,omitempty"`
	// InRoom answers ReplyTo in the room it was posted to rather than to
	// its author.
	InRoom bool `json:"in_room,omitempty"`
}

type SendResp struct {
	ID        string   `json:"id"`
	Seq       uint64   `json:"seq"`
	To        string   `json:"to"`
	Delivered []string `json:"delivered"`
}

type LeadReq struct {
	Action string `json:"action"` // take or handoff
	To     string `json:"to,omitempty"`
}

type StatusReq struct {
	WorkingOn string `json:"working_on"`
}

type InviteReq struct {
	Host string `json:"host"`
}

type InviteResp struct {
	Code      string   `json:"code"`
	Host      string   `json:"host"`
	ExpiresAt int64    `json:"expires_at"`
	Pin       string   `json:"pin"`
	URLs      []string `json:"urls,omitempty"`
}

type HostInfo struct {
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	Members   int    `json:"members"`
}

type ErrorResp struct {
	Error string `json:"error"`
}
