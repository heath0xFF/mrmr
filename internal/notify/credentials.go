package notify

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/gofrs/flock"
)

// DiscordCredentials keeps operator-provided URLs outside configuration history
// and the event database. A nil store preserves environment-only callers.
type DiscordCredentials struct{ Dir string }

type discordCredential struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
}

type DiscordConnection struct {
	Name       string
	Revision   string
	Configured bool
}

var discordReference = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var ErrDiscordConflict = errors.New("Discord connection changed. Reload connection status before replacing it.")

func DefaultDiscordCredentials() *DiscordCredentials {
	dir, err := os.UserConfigDir()
	if err != nil || !filepath.IsAbs(dir) {
		// Environment-only installations need no home directory. They can keep
		// running, but enrollment must fail rather than write into the repository.
		return nil
	}
	return &DiscordCredentials{Dir: filepath.Join(dir, "mrmr", "discord")}
}

func ValidateDiscordConnection(name, webhook string) error {
	if !discordReference.MatchString(name) {
		return fmt.Errorf("Discord connection name must use 1–64 letters, numbers, or underscores and cannot start with a number")
	}
	_, err := discordURL(webhook)
	return err
}

func privateDiscordFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("Discord credential files must be regular private files (mode 0600), not symlinks")
	}
	return nil
}

func (s *DiscordCredentials) directory() error {
	if s == nil || !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("private Discord credential directory is unavailable")
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("Discord credential directory must be a real private directory (mode 0700)")
	}
	return nil
}

func (s *DiscordCredentials) read() (map[string]discordCredential, error) {
	records := map[string]discordCredential{}
	if s == nil {
		return records, nil
	}
	if err := s.directory(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return records, nil
		}
		return nil, fmt.Errorf("cannot access private Discord credentials")
	}
	path := filepath.Join(s.Dir, "credentials.json")
	if err := privateDiscordFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return records, nil
		}
		return nil, fmt.Errorf("cannot access private Discord credentials")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read private Discord credentials")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 64<<10 {
		return nil, fmt.Errorf("Discord credential file exceeds 64 KiB or cannot be inspected")
	}
	decoder := json.NewDecoder(io.LimitReader(f, (64<<10)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&records); err != nil || records == nil || len(records) > 64 {
		return nil, fmt.Errorf("invalid private Discord credential file")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid private Discord credential file")
	}
	for name, record := range records {
		if ValidateDiscordConnection(name, record.URL) != nil || len(record.Revision) != 26 || !discordReference.MatchString(record.Revision) {
			return nil, fmt.Errorf("invalid private Discord credential record")
		}
	}
	return records, nil
}

// Resolve is called at dispatch, after the shadow gate. An unreadable store must
// fail closed rather than silently reverting to an older environment destination.
func (s *DiscordCredentials) Resolve(name string) (string, error) {
	records, err := s.read()
	if err != nil {
		return "", err
	}
	if record, ok := records[name]; ok {
		return record.URL, nil
	}
	return os.Getenv(name), nil
}

// Connections returns presence and independent revision IDs, never URL values
// or hashes of credentials. Saved, unused connections remain available for reuse.
func (s *DiscordCredentials) Connections(references []string) ([]DiscordConnection, error) {
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, name := range references {
		names[name] = true
	}
	for name := range records {
		names[name] = true
	}
	connections := []DiscordConnection{}
	for name := range names {
		record, stored := records[name]
		connections = append(connections, DiscordConnection{Name: name, Revision: record.Revision, Configured: stored || os.Getenv(name) != ""})
	}
	sort.Slice(connections, func(i, j int) bool { return connections[i].Name < connections[j].Name })
	return connections, nil
}

func (s *DiscordCredentials) Save(name, webhook, expected string) (string, error) {
	if err := ValidateDiscordConnection(name, webhook); err != nil {
		return "", err
	}
	if s == nil || !filepath.IsAbs(s.Dir) {
		return "", fmt.Errorf("private Discord credential directory is unavailable")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create private Discord credential directory")
	}
	if err := s.directory(); err != nil {
		return "", fmt.Errorf("cannot access private Discord credential directory")
	}
	// A filesystem lock also protects against another mrmr process using this
	// service account. Reads see either complete version through atomic rename.
	lockPath := filepath.Join(s.Dir, "credentials.lock")
	if err := privateDiscordFile(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cannot access Discord credential lock")
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	defer lock.Close()
	if ok, err := lock.TryLock(); err != nil || !ok {
		return "", fmt.Errorf("Discord credentials are busy or cannot be locked; retry shortly")
	}
	records, err := s.read()
	if err != nil {
		return "", err
	}
	if records[name].Revision != expected {
		return "", ErrDiscordConflict
	}
	id := "c" + rand.Text()[:25]
	records[name] = discordCredential{URL: webhook, Revision: id}
	data, err := json.Marshal(records)
	if err != nil || len(data) > 64<<10 || len(records) > 64 {
		return "", fmt.Errorf("Discord credential store exceeds 64 connections or 64 KiB")
	}
	f, err := os.CreateTemp(s.Dir, ".credentials-*")
	if err != nil {
		return "", fmt.Errorf("cannot create private Discord credential file")
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
		err = os.Rename(f.Name(), filepath.Join(s.Dir, "credentials.json"))
	}
	if err != nil {
		return "", fmt.Errorf("cannot persist Discord credentials; previous connections are unchanged")
	}
	// After rename the new destination is visible, even if directory sync fails.
	// Return its revision so management can record that uncertain durability.
	dir, err := os.Open(s.Dir)
	if err == nil {
		defer dir.Close()
		err = dir.Sync()
	}
	if err != nil {
		return id, fmt.Errorf("Discord connection saved but directory sync failed; reload status before retrying")
	}
	return id, nil
}
