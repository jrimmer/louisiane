package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/louisiane/internal/dash"
)

// Owner authority comes only from the dashboard's login: not from the admin
// socket, whose file permissions can't tell the owner from agents running as
// the same user.
func TestOnlyTheDashboardLoginSpeaksAsOwner(t *testing.T) {
	r, c := newRelay(t)
	hash, err := dash.HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.DashAuth, r.cfg.DashUser, r.cfg.DashHash = true, "owner", hash
	join(t, r, c, "desk", "k/a", "a", "claude")
	h := r.Handler(nil)
	send := func(admin bool, user, pass string) int {
		req := httptest.NewRequest("POST", "/owner/send", strings.NewReader(`{"to":"#all","body":"freeze"}`))
		if admin {
			req = req.WithContext(context.WithValue(req.Context(), adminKey, true))
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := send(true, "", ""); got != http.StatusForbidden {
		t.Fatalf("admin socket spoke as owner: %d", got)
	}
	if got := send(false, "owner", "wrong"); got != http.StatusForbidden {
		t.Fatalf("wrong password spoke as owner: %d", got)
	}
	if got := send(false, "owner", "secret"); got != http.StatusOK {
		t.Fatalf("the owner's login was refused: %d", got)
	}
	// A page on another site posting with the browser's saved login.
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"},
		{"Origin": "https://evil.example", "Content-Type": "application/json"},
		{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
		{"Origin": "https://example.com", "Sec-Fetch-Site": "same-origin", "Content-Type": "text/plain"},
	} {
		req := httptest.NewRequest("POST", "https://example.com/owner/send", strings.NewReader(`{"to":"#all","body":"freeze"}`))
		req.SetBasicAuth("owner", "secret")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-site post %v got %d", hdr, w.Code)
		}
	}
	// The dashboard's own page posting.
	req := httptest.NewRequest("POST", "https://example.com/owner/send", strings.NewReader(`{"to":"#all","body":"freeze"}`))
	req.SetBasicAuth("owner", "secret")
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the dashboard's own post got %d: %s", w.Code, w.Body)
	}
	r.cfg.DashAuth = false
	if got := send(false, "owner", "secret"); got != http.StatusForbidden {
		t.Fatalf("spoke as owner with the login off: %d", got)
	}
}
