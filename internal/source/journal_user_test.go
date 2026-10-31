package source

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// User-scoped readers still advance the global cursor, but ownership and unit
// selection must be checked before any record can reach a model or storage.
func TestUserJournalOwnershipAndManagerRecords(t *testing.T) {
	j := testJournal(t)
	j.cfg.Scope = "user"
	base := map[string]string{"__CURSOR": "c", "__REALTIME_TIMESTAMP": "1770000000000000", "_SYSTEMD_UNIT": "user@1000.service", "_SYSTEMD_USER_UNIT": "example.service", "_UID": strconv.Itoa(os.Getuid()), "PRIORITY": "4", "MESSAGE": "Synthetic warning"}
	cases := []struct {
		name     string
		changes  map[string]string
		selected bool
	}{
		{"own unit", nil, true},
		{"other user", map[string]string{"_UID": strconv.Itoa(os.Getuid() + 1)}, false},
		{"missing uid", map[string]string{"_UID": ""}, false},
		{"other unit", map[string]string{"_SYSTEMD_USER_UNIT": "other.service"}, false},
		{"system unit", map[string]string{"_SYSTEMD_USER_UNIT": "", "_SYSTEMD_UNIT": "example.service"}, false},
		{"untrusted selector", map[string]string{"_SYSTEMD_USER_UNIT": "other.service", "USER_UNIT": "example.service", "UNIT": "example.service"}, false},
		{"manager", map[string]string{"_SYSTEMD_USER_UNIT": "init.scope", "_COMM": "systemd", "_EXE": "/usr/lib/systemd/systemd", "USER_UNIT": "example.service"}, true},
		{"fake manager", map[string]string{"_SYSTEMD_USER_UNIT": "init.scope", "_COMM": "systemd", "_EXE": "/tmp/systemd", "USER_UNIT": "example.service"}, false},
		{"other manager uid", map[string]string{"_SYSTEMD_USER_UNIT": "init.scope", "_COMM": "systemd", "_EXE": "/usr/lib/systemd/systemd", "USER_UNIT": "example.service", "_UID": strconv.Itoa(os.Getuid() + 1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			for k, v := range tc.changes {
				values[k] = v
			}
			data, _ := json.Marshal(values)
			var record journalRecord
			json.Unmarshal(data, &record)
			e, selected, err := j.normalize(record, "c")
			if err != nil || selected != tc.selected {
				t.Fatalf("selected=%v err=%v", selected, err)
			}
			if selected && (e.Subject != "example.service" || e.Data["scope"] != "user" || e.Data["uid"] != os.Getuid()) {
				t.Fatal("lost user ownership in normalized event")
			}
			j.cfg.Scope = "system"
			if _, selected, err := j.normalize(record, "c"); err != nil || selected {
				t.Fatal("user record crossed into system scope")
			}
			j.cfg.Scope = "user"
		})
	}
}

func TestUserJournalPollKeepsCursorAndSkipsOtherUsers(t *testing.T) {
	j := testJournal(t)
	j.cfg.Scope = "user"
	endpoint, calls := modelMock(t)
	rt := testRuntime(t, endpoint)
	ctx := context.Background()
	if err := j.prepare(ctx, rt.DB); err != nil {
		t.Fatal(err)
	}
	record := func(cursor, uid string) string {
		var data map[string]string
		json.Unmarshal([]byte(journalJSON(cursor, "user@1000.service", "4", "Synthetic warning")), &data)
		data["_SYSTEMD_USER_UNIT"] = "example.service"
		data["_UID"] = uid
		b, _ := json.Marshal(data)
		return string(b) + "\n"
	}
	batch := journalJSON("anchor", "system.service", "6", "Old") + record("foreign", strconv.Itoa(os.Getuid()+1)) + record("own", strconv.Itoa(os.Getuid()))
	journalFile(t, j, "batch", batch)
	if err := j.poll(ctx, rt); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || countEvents(t, rt) != 1 {
		t.Fatal("ownership filter did not bound model work")
	}
	if cursor, err := rt.DB.Cursor(j.cfg.Name); err != nil || cursor != "own" {
		t.Fatal("user cursor did not advance")
	}
	args, err := os.ReadFile(strings.TrimSuffix(j.path, "journalctl") + "args")
	if err != nil || !strings.Contains(string(args), "_UID") || !strings.Contains(string(args), "USER_UNIT") {
		t.Fatal("journal command omitted ownership fields")
	}
}
