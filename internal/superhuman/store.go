// Package superhuman owns the one Superhuman connection. Credentials belong to
// mrmr, not an agent harness, and never enter event storage or flow definitions.
package superhuman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
)

var ErrLoginRequired = errors.New("Superhuman sign-in required: run mrmr auth login superhuman")

// DefaultDir keeps credentials outside the repository and the event database.
// An explicit state directory allows a service account to own its own grant.
func DefaultDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(dir, "mrmr", "superhuman"), nil
}

type credentialRecord struct {
	Version  int           `json:"version"`
	Resource string        `json:"resource"`
	Config   oauth2.Config `json:"oauth"`
	Token    oauth2.Token  `json:"token"`
}

type credentialStore struct {
	dir  string
	lock *flock.Flock
}

func openStore(dir string) (*credentialStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("credential directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot create credential directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("credential directory must be a real, private directory (mode 0700)")
	}
	lockPath := filepath.Join(dir, "credentials.lock")
	if err := privateFile(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		lock.Close()
		return nil, errors.New("Superhuman credentials are in use or cannot be locked; stop the other mrmr connection first")
	}
	// ponytail: one process owns the grant for its connection lifetime. Use
	// per-refresh locking only if concurrent clients become a real requirement.
	// OS locks release on process death, unlike a stale pid/lock directory.
	return &credentialStore{dir: dir, lock: lock}, nil
}

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("credential files must be regular private files (mode 0600), not symlinks")
	}
	return nil
}

func (s *credentialStore) read() (*credentialRecord, error) {
	path := filepath.Join(s.dir, "credentials.json")
	if err := privateFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrLoginRequired
		}
		return nil, errors.New("cannot read private Superhuman credentials")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open Superhuman credentials")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 1<<20 {
		return nil, errors.New("Superhuman credential file exceeds its size bound or cannot be inspected")
	}
	var record credentialRecord
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.New("invalid Superhuman credential file")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid Superhuman credential file suffix")
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *credentialRecord) validate() error {
	// A tampered or accidentally copied state file must not redirect a refresh
	// token to another service. Login metadata is also constrained by the client.
	if r.Version != 1 || r.Resource != endpoint || r.Config.ClientID == "" ||
		r.Config.Endpoint.TokenURL != tokenEndpoint || r.Token.AccessToken == "" ||
		r.Token.RefreshToken == "" || r.Token.Expiry.IsZero() || !strings.EqualFold(r.Token.TokenType, "Bearer") {
		return errors.New("invalid or non-refreshable Superhuman credentials; sign in again")
	}
	return nil
}

func (s *credentialStore) write(record *credentialRecord) error {
	if err := record.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("cannot encode Superhuman credentials")
	}
	// Rename within the same private directory preserves the previous grant on
	// pre-rename failures. Sync before returning a rotated token to any caller.
	f, err := os.CreateTemp(s.dir, ".credentials-*")
	if err != nil {
		return errors.New("cannot create private credential file")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "credentials.json"))
	}
	if err != nil {
		return errors.New("cannot persist Superhuman credentials")
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return errors.New("cannot sync credential directory")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("cannot sync credential directory")
	}
	return nil
}

// persistingTokens serializes refreshes within this process; the store's lock
// serializes processes. A rotated refresh token is saved before it is used.
// If saving fails, retain it in memory and retry the save, never re-use the old
// refresh token. A crash between remote rotation and local persistence still
// requires a new sign-in; no local transaction can eliminate that window.
type persistingTokens struct {
	mu     sync.Mutex
	ctx    context.Context
	store  *credentialStore
	record credentialRecord
	dirty  bool
}

func (s *persistingTokens) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		if err := s.store.write(&s.record); err != nil {
			return nil, err
		}
		s.dirty = false
	}
	token, err := s.record.Config.TokenSource(s.ctx, &s.record.Token).Token()
	if err != nil {
		// oauth2.RetrieveError includes the remote body, which may echo tokens.
		return nil, safeError("refresh Superhuman credentials", err)
	}
	old := &s.record.Token
	if token.AccessToken != old.AccessToken || token.RefreshToken != old.RefreshToken || !token.Expiry.Equal(old.Expiry) {
		s.record.Token = *token
		s.dirty = true
		if err := s.store.write(&s.record); err != nil {
			return nil, err
		}
		s.dirty = false
	}
	copy := *token
	return &copy, nil
}
