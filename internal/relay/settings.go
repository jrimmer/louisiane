package relay

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Settings is relay.json in the state directory: what `lal relay init`
// chose. Command-line flags override it.
type Settings struct {
	Listen   string   `json:"listen,omitempty"`
	Names    []string `json:"names,omitempty"`
	DashUser string   `json:"dash_user,omitempty"`
}

const (
	settingsFile = "relay.json"
	// HashFile holds the bcrypt hash of the dashboard password. The relay
	// rereads it when it changes, so setting the password needs no restart.
	HashFile = "dash.hash"
)

// LoadSettings reads dir/relay.json; a missing file is empty settings.
func LoadSettings(dir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Save writes dir/relay.json.
func (s Settings) Save(dir string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(dir, settingsFile, append(b, '\n'))
}

// WriteDashHash sets the dashboard password's hash in dir.
func WriteDashHash(dir, hash string) error {
	return writePrivate(dir, HashFile, []byte(hash+"\n"))
}

// HasDashHash reports whether a dashboard password has been set in dir.
func HasDashHash(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, HashFile))
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// writePrivate replaces dir/name atomically with mode 0600, owned like dir
// (so root can set up a relay that runs as its own user).
func writePrivate(dir, name string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if st, err := os.Stat(dir); err == nil && os.Geteuid() == 0 {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			os.Chown(tmp, int(sys.Uid), int(sys.Gid))
		}
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// dashHash is the dashboard password's hash: from the command line or
// environment if given, otherwise dash.hash, reread when it changes. Empty
// means no password is set and the dashboard stays locked.
func (r *Relay) dashHash() string {
	if r.cfg.DashHash != "" {
		return r.cfg.DashHash
	}
	path := filepath.Join(r.cfg.StateDir, HashFile)
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	r.hashMu.Lock()
	defer r.hashMu.Unlock()
	if !st.ModTime().Equal(r.hashAt) {
		if b, err := os.ReadFile(path); err == nil {
			r.hash, r.hashAt = strings.TrimSpace(string(b)), st.ModTime()
		}
	}
	return r.hash
}

// hashState is the cached dash.hash.
type hashState struct {
	hash   string
	hashAt time.Time
}
