package client

import "testing"

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:jrimmer/lal.git":              "github.com/jrimmer/lal",
		"https://github.com/jrimmer/lal":              "github.com/jrimmer/lal",
		"ssh://git@git.example.lan:2222/Team/API.git": "git.example.lan/team/api",
		"/srv/git/local.git":                          "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
