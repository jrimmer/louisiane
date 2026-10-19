package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// hostStore keeps the joined hosts and the hashes of their tokens in
// hosts.json. It is the relay's only durable state besides its
// certificate and the event log.
type hostStore struct {
	path  string
	mu    sync.Mutex
	Hosts []hostRec `json:"hosts"`
}

type hostRec struct {
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"`
	CreatedAt int64  `json:"created_at"`
}

func loadHosts(path string) (*hostStore, error) {
	s := &hostStore{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, s)
}

func (s *hostStore) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func hashSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// lookup returns the host a token belongs to.
func (s *hostStore) lookup(token string) (string, bool) {
	h := hashSecret(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.Hosts {
		if subtle.ConstantTimeCompare([]byte(rec.TokenHash), []byte(h)) == 1 {
			return rec.Name, true
		}
	}
	return "", false
}

// add gives host a new token, replacing any it had.
func (s *hostStore) add(host string) (string, error) {
	token := "lal_" + randID("", 24)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := hostRec{Name: host, TokenHash: hashSecret(token), CreatedAt: time.Now().UnixMilli()}
	replaced := false
	for i := range s.Hosts {
		if s.Hosts[i].Name == host {
			s.Hosts[i], replaced = rec, true
		}
	}
	if !replaced {
		s.Hosts = append(s.Hosts, rec)
	}
	return token, s.save()
}

func (s *hostStore) remove(host string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Hosts {
		if s.Hosts[i].Name == host {
			s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
			return true, s.save()
		}
	}
	return false, nil
}

func (s *hostStore) list() []hostRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]hostRec(nil), s.Hosts...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *hostStore) names() []string {
	var out []string
	for _, h := range s.list() {
		out = append(out, h.Name)
	}
	return out
}

type invite struct {
	host    string
	expires int64
}

// newInvite mints a one-time code, valid ten minutes, for host.
func (r *Relay) newInvite(host string) (string, int64) {
	raw := strings.ToUpper(randID("", 5))[:8]
	code := raw[:4] + "-" + raw[4:]
	exp := r.ms() + 10*time.Minute.Milliseconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	for h, inv := range r.invites {
		if inv.host == host || inv.expires < r.ms() {
			delete(r.invites, h)
		}
	}
	r.invites[hashSecret(code)] = invite{host: host, expires: exp}
	return code, exp
}

// redeem trades a code for a host token.
func (r *Relay) redeem(code string) (string, string, error) {
	h := hashSecret(strings.ToUpper(strings.TrimSpace(code)))
	r.mu.Lock()
	inv, ok := r.invites[h]
	delete(r.invites, h)
	r.mu.Unlock()
	if !ok || inv.expires < r.ms() {
		return "", "", errf(403, "that invite code is unknown or expired; mint another with `lal invite HOST` on the relay host")
	}
	token, err := r.hosts.add(inv.host)
	return inv.host, token, err
}
