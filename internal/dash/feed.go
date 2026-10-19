package dash

import (
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/proto"
)

// FeedData is the catch-up page's content.
type FeedData struct {
	Events  []proto.Event
	Since   string
	Room    string
	Who     string
	Who2    proto.Who
	CanSend bool
}

var feedTmpl = template.Must(template.New("feed").Funcs(template.FuncMap{
	"clock": func(ms int64) string { return time.UnixMilli(ms).Format("Mon 15:04:05") },
	"where": func(e proto.Event) string {
		switch {
		case e.Room != "":
			return "#" + e.Room
		case e.To != "":
			return e.To
		}
		return ""
	},
	"lines": func(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") },
	"owner": func(s string) bool { return s == "owner" },
	"list":  func(xs ...string) []string { return xs },
}).Parse(feedHTML))

// Feed writes the catch-up page.
func Feed(w io.Writer, d FeedData) error {
	if d.Since == "" {
		d.Since = "2h"
	}
	return feedTmpl.Execute(w, d)
}

const feedHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>louisiane feed</title>
<link rel="stylesheet" href="/static/fonts/jetbrains-mono.css">
<style>
:root { --bg:#0c0c0d; --fg:#c9c9c9; --dim:#6b6b6b; --frame:#3a3a40; --title:#6FD8E8; --ok:#6EF59A;
  --warn:#FFC46B; --bad:#FF7A85; --owner:#B09AFF; --id:#6FD8E8; --me:#FFC46B; }
html, body { background: var(--bg); color: var(--fg); margin: 0; }
body { font: 13px/1.5 "JetBrains Mono", ui-monospace, monospace; }
main { max-width: 100ch; margin: 0 auto; padding: 1rem; }
a { color: var(--title); }
nav { color: var(--dim); margin-bottom: 1rem; display: flex; flex-wrap: wrap; gap: .25rem 1rem; }
nav b { color: var(--fg); }
.ev { border-left: 2px solid var(--frame); padding: .1rem 0 .1rem .75rem; margin: .5rem 0; }
.ev.owner { border-color: var(--me); }
.ev.leader, .ev.status { color: var(--dim); }
.h { color: var(--dim); }
.from { color: var(--owner); }
.ev.owner .from { color: var(--me); font-weight: 700; }
.to { color: var(--id); }
.body { white-space: pre-wrap; word-break: break-word; }
.ask { color: var(--warn); }
form { border: 1px solid var(--frame); padding: .75rem; margin: 1rem 0; display: grid; gap: .5rem; }
input, textarea, button { font: inherit; background: #141416; color: var(--fg); border: 1px solid var(--frame); padding: .35rem .5rem; }
textarea { min-height: 4.5em; resize: vertical; }
button { cursor: pointer; justify-self: start; }
#result { color: var(--dim); }
</style>
</head>
<body>
<main>
<nav>
  <a href="/">dashboard</a>
  <span>since:
  {{range $d := (list "30m" "2h" "8h" "24h" "72h")}}<a href="?since={{$d}}{{if $.Room}}&room={{$.Room}}{{end}}{{if $.Who}}&who={{$.Who}}{{end}}">{{if eq $d $.Since}}<b>{{$d}}</b>{{else}}{{$d}}{{end}}</a> {{end}}</span>
  <span>rooms: <a href="?since={{.Since}}">everything</a>
  {{range .Who2.Rooms}} <a href="?since={{$.Since}}&room={{.Name}}">#{{.Name}}</a>{{end}}</span>
</nav>
{{if .CanSend}}
<form id="send">
  <input name="to" value="{{if .Room}}#{{.Room}}{{else}}#all{{end}}" aria-label="to" placeholder="#all, #project, project, project/member">
  <textarea name="body" aria-label="message" placeholder="A message from you, the owner. Agents act on it with your authority."></textarea>
  <label><input type="checkbox" name="ask"> asks a question</label>
  <button type="submit">send as owner</button>
  <span id="result"></span>
</form>
<script>
document.getElementById('send').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target, out = document.getElementById('result');
  const res = await fetch('/owner/send', {method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({to: f.to.value, body: f.body.value, ask: f.ask.checked})});
  const j = await res.json().catch(() => ({}));
  if (res.ok) { out.textContent = 'sent ' + j.id + ' to ' + (j.delivered || []).join(', '); f.body.value = ''; setTimeout(() => location.reload(), 800); }
  else { out.textContent = j.error || res.statusText; }
});
</script>
{{end}}
{{if not .Events}}<p class="h">Nothing in this window.</p>{{end}}
{{range .Events}}
{{if eq .Type "msg"}}
<div class="ev{{if owner .From}} owner{{end}}">
  <div class="h">{{clock .At}} · <span class="from">{{.From}}</span> → <span class="to">{{where .}}</span>{{if .Ask}} <span class="ask">asks</span>{{end}} · {{.ID}}{{if .ReplyTo}} · re {{.ReplyTo}}{{end}} · reached {{len .Delivered}}</div>
  <div class="body">{{.Body}}</div>
</div>
{{else if eq .Type "leader"}}
<div class="ev leader">{{clock .At}} · {{.Member}} leads {{.Project}} (term {{.Term}}, {{.Action}}{{if .Body}} {{.Body}}{{end}})</div>
{{else if eq .Type "status"}}
<div class="ev status">{{clock .At}} · {{.Member}} is working on: {{.Body}}</div>
{{else}}
<div class="ev status">{{clock .At}} · {{.Type}} {{.Member}}{{.From}} {{.Body}}</div>
{{end}}
{{end}}
</main>
</body>
</html>`
