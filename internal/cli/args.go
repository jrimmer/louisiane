package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// args splits a command line into flags and positionals. Flags may come
// anywhere; "--" ends them. Flags named in valued take the next argument
// (or "--flag=value").
type args struct {
	flags map[string][]string
	pos   []string
}

func parse(in []string, valued ...string) (*args, error) {
	takes := map[string]bool{}
	for _, v := range valued {
		takes[v] = true
	}
	a := &args{flags: map[string][]string{}}
	for i := 0; i < len(in); i++ {
		s := in[i]
		if s == "--" {
			a.pos = append(a.pos, in[i+1:]...)
			break
		}
		if !strings.HasPrefix(s, "--") || s == "-" {
			a.pos = append(a.pos, s)
			continue
		}
		name, val, hasVal := strings.Cut(s[2:], "=")
		if takes[name] && !hasVal {
			if i+1 >= len(in) {
				return nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = in[i]
		}
		a.flags[name] = append(a.flags[name], val)
	}
	return a, nil
}

func (a *args) has(name string) bool { _, ok := a.flags[name]; return ok }

func (a *args) str(name, def string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

func (a *args) all(name string) []string { return a.flags[name] }

func (a *args) dur(name string, def time.Duration) (time.Duration, error) {
	v := a.str(name, "")
	if v == "" {
		return def, nil
	}
	return time.ParseDuration(v)
}

// text joins positionals from i as a message; "-" reads stdin.
func (a *args) text(i int) (string, error) {
	if len(a.pos) <= i {
		return "", fmt.Errorf("no message")
	}
	if len(a.pos) == i+1 && a.pos[i] == "-" {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimRight(string(b), "\n"), err
	}
	return strings.Join(a.pos[i:], " "), nil
}
