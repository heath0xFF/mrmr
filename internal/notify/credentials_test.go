package notify

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

func TestDiscordCredentialsWithoutHomePreserveEnvironmentOnly(t *testing.T) {
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "home"} {
		t.Setenv(key, "")
	}
	t.Setenv("TEST_ENV_ONLY_DISCORD", syntheticWebhook)
	store := DefaultDiscordCredentials()
	if store != nil {
		t.Fatal("missing account directory should not create a relative store")
	}
	if value, err := store.Resolve("TEST_ENV_ONLY_DISCORD"); err != nil || value != syntheticWebhook {
		t.Fatal("environment-only setup broke")
	}
	if _, err := store.Save("alerts", syntheticWebhook, ""); err == nil {
		t.Fatal("enrollment without a private directory succeeded")
	}
}

func TestDiscordCredentialPersistenceAndConflict(t *testing.T) {
	const name = "TEST_DISCORD_CONNECTION"
	t.Setenv(name, syntheticWebhook+"environment")
	s := &DiscordCredentials{Dir: filepath.Join(t.TempDir(), "discord")}
	if value, err := s.Resolve(name); err != nil || value != os.Getenv(name) {
		t.Fatal("missing store did not preserve environment fallback")
	}
	first, err := s.Save(name, syntheticWebhook, "")
	if err != nil {
		t.Fatal(err)
	}
	// A new handle models restart; neither process environment nor YAML is edited.
	reopened := &DiscordCredentials{Dir: s.Dir}
	if value, err := reopened.Resolve(name); err != nil || value != syntheticWebhook || os.Getenv(name) != syntheticWebhook+"environment" {
		t.Fatal("persisted credentials did not take precedence without changing environment")
	}
	connections, err := reopened.Connections([]string{name})
	if err != nil || len(connections) != 1 || !connections[0].Configured || connections[0].Revision != first {
		t.Fatalf("metadata: %v", err)
	}
	metadata, _ := json.Marshal(connections)
	if strings.Contains(string(metadata), syntheticWebhook) {
		t.Fatal("metadata leaked a URL")
	}
	if _, err := s.Save(name, syntheticWebhook+"new", ""); !errors.Is(err, ErrDiscordConflict) {
		t.Fatal("stale operator overwrote a connection")
	}
	second, err := s.Save(name, syntheticWebhook+"new", first)
	if err != nil || second == first {
		t.Fatalf("rotation failed: %v", err)
	}
	for _, file := range []string{"credentials.json", "credentials.lock"} {
		info, err := os.Stat(filepath.Join(s.Dir, file))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("credential file is not private: %s", file)
		}
	}
	info, _ := os.Stat(s.Dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("credential directory is not private")
	}
	lock := flock.New(filepath.Join(s.Dir, "credentials.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := s.Save(name, syntheticWebhook, second); err == nil {
		t.Fatal("writer ignored another process's lock")
	}
}

func TestDiscordCredentialStoreFailsClosed(t *testing.T) {
	for _, kind := range []string{"public directory", "directory symlink", "public file", "file symlink", "lock symlink", "malformed", "oversized", "invalid URL", "null"} {
		t.Run(kind, func(t *testing.T) {
			const name = "TEST_PRIVATE_DISCORD"
			t.Setenv(name, syntheticWebhook+"old")
			s := &DiscordCredentials{Dir: filepath.Join(t.TempDir(), "discord")}
			id, err := s.Save(name, syntheticWebhook, "")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Dir, "credentials.json")
			switch kind {
			case "public directory":
				err = os.Chmod(s.Dir, 0755)
			case "directory symlink":
				err = os.Rename(s.Dir, s.Dir+"-real")
				if err == nil {
					err = os.Symlink(s.Dir+"-real", s.Dir)
				}
			case "public file":
				err = os.Chmod(path, 0644)
			case "file symlink", "lock symlink":
				if kind == "lock symlink" {
					path = filepath.Join(s.Dir, "credentials.lock")
				}
				err = os.Rename(path, path+"-real")
				if err == nil {
					err = os.Symlink(path+"-real", path)
				}
			case "malformed":
				err = os.WriteFile(path, []byte(syntheticWebhook), 0600)
			case "oversized":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", (64<<10)+1)), 0600)
			case "invalid URL":
				data, _ := json.Marshal(map[string]discordCredential{name: {URL: "http://127.0.0.1/private", Revision: id}})
				err = os.WriteFile(path, data, 0600)
			case "null":
				err = os.WriteFile(path, []byte("null"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "lock symlink" {
				if value, err := s.Resolve(name); err == nil || value != "" || strings.Contains(err.Error(), syntheticWebhook) {
					t.Fatal("unsafe store fell back to environment or exposed a credential")
				}
			}
			if _, err := s.Save(name, syntheticWebhook+"replacement", id); err == nil || strings.Contains(err.Error(), syntheticWebhook) {
				t.Fatal("unsafe store was overwritten or leaked a credential")
			}
		})
	}
}

func TestDiscordEnrollmentRejectsUnboundedOrInvalidDestinations(t *testing.T) {
	s := &DiscordCredentials{Dir: filepath.Join(t.TempDir(), "discord")}
	for _, value := range []string{"", "http://discord.com/api/webhooks/1/token", "https://evil.invalid/api/webhooks/1/token", syntheticWebhook + "?wait=true", syntheticWebhook + "#fragment", syntheticWebhook + strings.Repeat("x", 4096)} {
		if _, err := s.Save("alerts", value, ""); err == nil || (value != "" && strings.Contains(err.Error(), value)) {
			t.Fatal("invalid enrollment accepted or echoed")
		}
	}
	for _, name := range []string{"../escape", "", "1name", strings.Repeat("a", 65)} {
		if _, err := s.Save(name, syntheticWebhook, ""); err == nil {
			t.Fatal("invalid connection name accepted")
		}
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("invalid enrollment created storage")
	}
}
