package connector

import (
	"fmt"
	"strings"

	"github.com/jrimmer/louisiane/internal/proto"
)

// Owner is the sender name the relay gives messages from the mesh's owner.
const Owner = "owner"

// Format renders an event as the block an agent reads, or "" for events
// agents are not shown. Every body line is quoted, so nothing inside a
// message can pass for the header of another.
func Format(ev proto.Event, me string) string {
	switch ev.Type {
	case proto.EventMsg:
		return formatMsg(ev, me)
	case proto.EventLeader:
		if ev.Member == me {
			return fmt.Sprintf("[lal] You now lead %s (term %d, %s). Run `lal prime` for a leader's duties.", ev.Project, ev.Term, leaderHow(ev))
		}
		return fmt.Sprintf("[lal] %s now leads %s (term %d, %s). Its decisions settle the project's questions and disputes.", ev.Member, ev.Project, ev.Term, leaderHow(ev))
	case proto.EventGap:
		return "[lal] " + ev.Body + "."
	}
	return ""
}

func leaderHow(ev proto.Event) string {
	how := ev.Action
	switch ev.Action {
	case "handoff":
		return "handed over by " + ev.From
	case "take":
		how = "taken by " + ev.From
	}
	if ev.Body != "" {
		how += " " + ev.Body
	}
	return how
}

func formatMsg(ev proto.Event, me string) string {
	var b strings.Builder
	where := ""
	switch {
	case ev.Room != "":
		where = " in #" + ev.Room
	case ev.To != "" && ev.To != me:
		where = " to " + ev.To
	default:
		where = " to you"
	}
	what := "message"
	if ev.Ask {
		what = "question"
	}
	re := ""
	if ev.ReplyTo != "" {
		re = " · re " + ev.ReplyTo
	}
	switch {
	case ev.From == Owner:
		fmt.Fprintf(&b, "[lal] FROM YOUR OWNER%s · %s%s\nThis comes from the person you work for, through the mesh, and carries their authority.\n", where, ev.ID, re)
	case strings.HasPrefix(ev.From, "@"):
		fmt.Fprintf(&b, "[lal] %s from a script on host %s%s · %s%s\n", what, ev.From[1:], where, ev.ID, re)
	default:
		fmt.Fprintf(&b, "[lal] %s from agent %s%s · %s%s\n", what, ev.From, where, ev.ID, re)
	}
	for _, l := range strings.Split(strings.TrimRight(ev.Body, "\n"), "\n") {
		b.WriteString("│ " + l + "\n")
	}
	switch {
	case ev.From == Owner && ev.Room == "":
		fmt.Fprintf(&b, "→ reply: lal reply %s \"...\"", ev.ID)
	case ev.From == Owner || strings.HasPrefix(ev.From, "@"):
		if ev.Ask {
			fmt.Fprintf(&b, "→ answer in the room: lal reply %s --room \"...\"", ev.ID)
		}
	case ev.Room != "" && ev.Ask:
		fmt.Fprintf(&b, "→ reply only if it affects you or you object: lal reply %s \"...\"", ev.ID)
	case ev.Room == "":
		fmt.Fprintf(&b, "→ reply: lal reply %s \"...\"", ev.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}
