package source

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
)

// The fake executable exercises actual exec/pipe/cancellation behavior on
// Linux and macOS, without journal permissions, real logs, or external tools
// beyond the platform shell. Production construction still requires Linux.
func testJournal(t *testing.T) *Journal {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("journal subprocess fixtures require a POSIX shell")
	}
	dir := t.TempDir()
	j := &Journal{path: filepath.Join(dir, "journalctl"), cfg: config.Source{
		Name: "journal", Type: "systemd-journal", Units: []string{"example.service"},
		Priority: "warning", Every: time.Minute, EventType: "system.service.warning",
	}}
	journalFile(t, j, "journalctl", `#!/bin/sh
set -eu
dir=$(dirname "$0")
printf '%s\n' "$@" > "$dir/args"
if [ -f "$dir/stderr" ]; then cat "$dir/stderr" >&2; fi
if [ -f "$dir/fail" ]; then exit 1; fi
case " $* " in
  *" --lines=1 "*) cat "$dir/tail" ;;
  *) cat "$dir/batch" ;;
esac
`)
	if err := os.Chmod(j.path, 0700); err != nil {
		t.Fatal(err)
	}
	journalFile(t, j, "tail", journalJSON("anchor", "example.service", "4", "old warning"))
	journalFile(t, j, "batch", journalJSON("anchor", "example.service", "4", "old warning"))
	return j
}

func journalFile(t *testing.T, j *Journal, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(j.path), name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func journalJSON(cursor, unit, priority, message string) string {
	b, _ := json.Marshal(map[string]string{
		"__CURSOR": cursor, "__REALTIME_TIMESTAMP": "1770000000000000",
		"_SYSTEMD_UNIT": unit, "PRIORITY": priority, "MESSAGE": message,
	})
	return string(b) + "\n"
}

func TestJournalCheckpointFilteringAndRecovery(t *testing.T) {
	j := testJournal(t)
	modelURL, calls := modelMock(t)
	rt := testRuntime(t, modelURL)
	ctx := context.Background()
	if err := j.prepare(ctx, rt.DB); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(t, rt); got != 0 {
		t.Fatal("bootstrap must not ingest historical warnings")
	}
	batch := journalJSON("anchor", "example.service", "4", "old") +
		journalJSON("other", "other.service", "3", "not selected") +
		journalJSON("info", "example.service", "6", "routine") +
		journalJSON("b", "example.service", "4", "warning") +
		journalJSON("c", "example.service", "3", "error")
	journalFile(t, j, "batch", batch)
	if _, err := rt.DB.Exec(`CREATE TRIGGER fail_journal BEFORE INSERT ON events
		WHEN NEW.dedup_key = 'c' BEGIN SELECT RAISE(FAIL, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := j.poll(ctx, rt); err == nil {
		t.Fatal("want event persistence failure")
	}
	if c, err := rt.DB.Cursor(j.cfg.Name); err != nil || c != "anchor" {
		t.Fatalf("failed batch cursor = %q, %v", c, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want only first selected record", calls.Load())
	}
	if _, err := rt.DB.Exec(`DROP TRIGGER fail_journal`); err != nil {
		t.Fatal(err)
	}
	// A newly constructed reader resumes the persisted checkpoint, not the
	// tail. The event already stored before failure must not cost inference.
	restarted := &Journal{path: j.path, cfg: j.cfg}
	if err := restarted.prepare(ctx, rt.DB); err != nil {
		t.Fatal(err)
	}
	if err := restarted.poll(ctx, rt); err != nil {
		t.Fatal(err)
	}
	if c, err := rt.DB.Cursor(j.cfg.Name); err != nil || c != "c" {
		t.Fatalf("recovered cursor = %q, %v", c, err)
	}
	if calls.Load() != 2 || countEvents(t, rt) != 2 {
		t.Fatal("only the two selected records should be interpreted/stored")
	}
	var id string
	if err := rt.DB.QueryRow(`SELECT id FROM events WHERE dedup_key = 'b'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ins, err := rt.DB.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if ins.Event.Subject != "example.service" || ins.Event.Data["message"] != "warning" ||
		ins.Event.Timestamp.UnixMicro() != 1770000000000000 || len(ins.Trace) == 0 {
		t.Fatalf("unexpected normalized event/trace: %+v", ins)
	}
	args, err := os.ReadFile(filepath.Join(filepath.Dir(j.path), "args"))
	if err != nil || !strings.Contains(string(args), "--lines=+101") || !strings.Contains(string(args), "--system") {
		t.Fatalf("must scan oldest system records: %s, %v", args, err)
	}
	// No matching warnings still advances through the system journal,
	// keeping an otherwise quiet source's checkpoint fresh.
	journalFile(t, j, "batch", journalJSON("c", "example.service", "3", "error")+journalJSON("quiet", "other.service", "6", "routine"))
	if err := j.poll(ctx, rt); err != nil {
		t.Fatal(err)
	}
	if c, _ := rt.DB.Cursor(j.cfg.Name); c != "quiet" || calls.Load() != 2 {
		t.Fatal("unselected records must advance without inference")
	}
}

func TestJournalRejectsUnreadableOrLostPosition(t *testing.T) {
	for _, fault := range []string{"warning", "exit", "empty", "malformed", "vacuumed", "oversized", "too many"} {
		t.Run(fault, func(t *testing.T) {
			j := testJournal(t)
			modelURL, calls := modelMock(t)
			rt := testRuntime(t, modelURL)
			if err := j.prepare(context.Background(), rt.DB); err != nil {
				t.Fatal(err)
			}
			journalFile(t, j, "batch", journalJSON("anchor", "example.service", "4", "old"))
			switch fault {
			case "warning":
				journalFile(t, j, "stderr", "private diagnostic must not escape")
			case "exit":
				journalFile(t, j, "fail", "")
			case "empty":
				journalFile(t, j, "batch", "")
			case "malformed":
				journalFile(t, j, "batch", "private malformed content")
			case "vacuumed":
				journalFile(t, j, "batch", journalJSON("nearby", "example.service", "4", "must not ingest"))
			case "oversized":
				journalFile(t, j, "batch", strings.Repeat("x", (4<<20)+1))
			case "too many":
				journalFile(t, j, "batch", strings.Repeat(journalJSON("anchor", "example.service", "4", "old"), journalBatchSize+2))
			}
			err := j.poll(context.Background(), rt)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("want safe error, got %v", err)
			}
			if c, _ := rt.DB.Cursor(j.cfg.Name); c != "anchor" || calls.Load() != 0 {
				t.Fatal("failure must preserve checkpoint and avoid inference")
			}
			if err := j.prepare(context.Background(), rt.DB); err == nil {
				t.Fatal("same failure must also block activation")
			}
		})
	}
}

func TestJournalNormalizationBoundaries(t *testing.T) {
	j := testJournal(t)
	var r journalRecord
	if err := json.Unmarshal([]byte(journalJSON("a", "init.scope", "3", "failed")), &r); err != nil {
		t.Fatal(err)
	}
	r["UNIT"], r["_PID"] = json.RawMessage(`"example.service"`), json.RawMessage(`"1"`)
	e, selected, err := j.normalize(r, "a")
	if err != nil || !selected || e.Subject != "example.service" {
		t.Fatal("system manager's unit transition must be selected")
	}
	r["_PID"] = json.RawMessage(`"2000"`)
	if _, selected, _ := j.normalize(r, "a"); selected {
		t.Fatal("arbitrary UNIT field must not bypass system-unit selection")
	}
	r["_SYSTEMD_UNIT"] = json.RawMessage(`"example.service"`)
	for _, msg := range []string{`null`, `[65,66]`, `""`} {
		r["MESSAGE"] = json.RawMessage(msg)
		e, _, err := j.normalize(r, "a")
		if err != nil || e.Data["message_omitted"] != true {
			t.Fatal("binary/oversized/empty messages must be marked omitted")
		}
	}
	r["MESSAGE"], _ = json.Marshal(strings.Repeat("é", 3000))
	e, _, _ = j.normalize(r, "a")
	if len(e.Data["message"].(string)) > 4096 || e.Data["message_truncated"] != true {
		t.Fatal("message must be bounded")
	}
	j.bootID = "current-boot"
	r["_BOOT_ID"], r["_PID"] = json.RawMessage(`"current-boot"`), json.RawMessage(strconv.Quote(strconv.Itoa(os.Getpid())))
	if _, selected, _ := j.normalize(r, "a"); selected {
		t.Fatal("own process logs must not feed back into the model")
	}
	r["_BOOT_ID"] = json.RawMessage(`"previous-boot"`)
	if _, selected, _ := j.normalize(r, "a"); !selected {
		t.Fatal("PID reuse across boots must not hide unrelated records")
	}
	r["__REALTIME_TIMESTAMP"] = json.RawMessage(`"bad"`)
	if _, _, err := j.normalize(r, "a"); err == nil {
		t.Fatal("invalid timestamp must not become now")
	}
}

func TestJournalOutputAndCancellationBounds(t *testing.T) {
	// io.Copy must not discover a promoted bytes.Buffer.ReadFrom method
	// that bypasses Write. A pipe forces the writer-side fast path check.
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); defer writer.Close(); _, _ = io.WriteString(writer, "12345") }()
	out := journalOutput{limit: 4}
	_, err := io.Copy(&out, reader)
	reader.Close()
	<-done
	if err == nil || out.buf.Len() > 4 {
		t.Fatal("output exceeded memory limit")
	}
	j := testJournal(t)
	journalFile(t, j, "journalctl", "#!/bin/sh\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := j.read(ctx, "__CURSOR", "--lines=1"); err == nil {
		t.Fatal("want canceled subprocess")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child was not killed and reaped promptly")
	}
	modelURL, _ := modelMock(t)
	rt := testRuntime(t, modelURL)
	if err := rt.DB.SetCursor(j.cfg.Name, "anchor"); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	stopped := make(chan struct{})
	go func() { defer close(stopped); j.Run(runCtx, rt) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

// Linux CI exercises the public constructor too; the rest of the suite
// intentionally runs on a development Mac without a real system journal.
func TestJournalLinuxConstructor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("public constructor requires Linux")
	}
	j := testJournal(t)
	t.Setenv("PATH", filepath.Dir(j.path)+string(os.PathListSeparator)+os.Getenv("PATH"))
	modelURL, _ := modelMock(t)
	rt := testRuntime(t, modelURL)
	prepared, err := NewJournal(context.Background(), j.cfg, rt.DB)
	if err != nil || prepared == nil {
		t.Fatalf("prepare built-in source: %v", err)
	}
	if c, err := rt.DB.Cursor(j.cfg.Name); err != nil || c != "anchor" {
		t.Fatalf("bootstrap cursor = %q, %v", c, err)
	}
}

func TestJournalPlatformGate(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Setenv("PATH", t.TempDir())
	}
	// Both cases fail before touching a DB: unsupported OS or no journalctl.
	if _, err := NewJournal(context.Background(), config.Source{Name: "journal"}, nil); err == nil {
		t.Fatal("want actionable platform/executable error")
	}
}
