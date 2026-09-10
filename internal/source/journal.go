package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	mruntime "github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

const journalBatchSize = 100

// Journal reads the local system journal only. It owns no background process:
// each bounded journalctl call is reaped before interpretation begins. Run's
// caller owns the single polling goroutine and cancels it on every exit path.
// No command, arguments, permissions, or cursor ever come from a model.
type Journal struct {
	cfg    config.Source
	path   string
	bootID string
}

// NewJournal checks platform, executable, journal access, and resume position
// before the daemon activates ANY sources. Reading the unfiltered system tail
// distinguishes an empty unit selection from an inaccessible journal. We do
// not suppress journalctl's permission warnings, even on a successful exit.
func NewJournal(ctx context.Context, cfg config.Source, db *storage.DB) (*Journal, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("source %q: systemd-journal requires Linux", cfg.Name)
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, fmt.Errorf("source %q: journalctl is required; install the systemd journal tools", cfg.Name)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, fmt.Errorf("source %q: read Linux boot identity: %w", cfg.Name, err)
	}
	j := &Journal{cfg: cfg, path: path, bootID: strings.ReplaceAll(strings.TrimSpace(string(boot)), "-", "")}
	if err := j.prepare(ctx, db); err != nil {
		return nil, fmt.Errorf("source %q: %w", cfg.Name, err)
	}
	return j, nil
}

func (j *Journal) prepare(ctx context.Context, db *storage.DB) error {
	cursor, err := db.Cursor(j.cfg.Name)
	if err != nil {
		return err
	}
	if cursor != "" {
		_, err := j.batch(ctx, cursor)
		return err
	}
	// First activation starts at the current global tail, not the latest
	// matching warning: an old warning must not turn into a fresh incident.
	// Only the cursor is requested; historical message text is unnecessary.
	records, err := j.read(ctx, "__CURSOR", "--lines=1")
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("no readable system journal entries; verify journald is running and this account has system journal read access")
	}
	cursor, err = records[0].cursor()
	if err != nil {
		return err
	}
	// Probe forward-scan support on fresh installations too, not only when
	// an existing checkpoint happens to exercise --lines=+N during startup.
	if _, err := j.batch(ctx, cursor); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return db.SetCursor(j.cfg.Name, cursor)
}

func (j *Journal) Run(ctx context.Context, rt *mruntime.Runtime) {
	ticker := time.NewTicker(j.cfg.Every)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := j.poll(ctx, rt); err != nil && ctx.Err() == nil {
			log.Printf("mrmr: journal %s: %v", j.cfg.Name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (j *Journal) poll(ctx context.Context, rt *mruntime.Runtime) error {
	cursor, err := rt.DB.Cursor(j.cfg.Name)
	if err != nil {
		return err
	}
	if cursor == "" {
		return fmt.Errorf("journal checkpoint missing; restart to establish a new tail")
	}
	records, err := j.batch(ctx, cursor)
	if err != nil {
		return err
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cursor, err = record.cursor()
		if err != nil {
			return err
		}
		e, selected, err := j.normalize(record, cursor)
		if err != nil {
			return err
		}
		if selected {
			if _, err := rt.Ingest(ctx, e); err != nil {
				// Preserve the old batch checkpoint. Already stored events
				// dedup on the next poll; runtime partial-work recovery is a
				// separate limitation, not something this cursor can repair.
				return fmt.Errorf("ingest journal event: %w", err)
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(records) == 0 {
		return nil
	}
	// Unselected records advance too: a quiet source must not retain an
	// ancient warning's cursor until vacuum removes it. Nothing from those
	// records is persisted except the final opaque journal position.
	return rt.DB.SetCursor(j.cfg.Name, cursor)
}

func (j *Journal) batch(ctx context.Context, cursor string) ([]journalRecord, error) {
	// journalctl can seek to a nearby record when a cursor was vacuumed.
	// Include the checkpoint and require an exact match before consuming
	// anything. --lines=+N reads the OLDEST next records, unlike -n N which
	// could jump to the tail and lose a backlog larger than one batch.
	// ponytail: scan 100 system records/tick and filter below; this preserves
	// exact checkpoint verification and progress through quiet selections.
	// Add indexed unit queries only if measured journal volume needs them.
	records, err := j.read(ctx, "__CURSOR,__REALTIME_TIMESTAMP,_SYSTEMD_UNIT,UNIT,_PID,PRIORITY,MESSAGE",
		"--cursor="+cursor, "--lines=+"+strconv.Itoa(journalBatchSize+1))
	if err != nil {
		return nil, err
	}
	if len(records) == 0 || records[0].text("__CURSOR") != cursor {
		return nil, fmt.Errorf("journal checkpoint unavailable (possibly rotated or vacuumed); refusing to skip records; use a new source name only if you intend to restart at the current tail")
	}
	return records[1:], nil
}

// journalRecord keeps fields untrusted. Journald may encode binary values or
// repeated fields as arrays and oversized values as null; neither should be
// coerced into a unit identity, severity, or executable argument.
type journalRecord map[string]json.RawMessage

func (r journalRecord) text(key string) string {
	var s string
	_ = json.Unmarshal(r[key], &s)
	return s
}

func (r journalRecord) cursor() (string, error) {
	c := r.text("__CURSOR")
	if len(c) == 0 || len(c) > 4096 || bytes.ContainsAny([]byte(c), "\x00\r\n") {
		return "", fmt.Errorf("journal record has an invalid cursor")
	}
	return c, nil
}

func (j *Journal) normalize(r journalRecord, cursor string) (event.Event, bool, error) {
	// A broad unit choice must not turn our own notifications/diagnostics
	// into new model work. Journald's trusted PID cannot be set by a client.
	// Include boot identity: a persisted backlog may contain an unrelated
	// process with this PID from a previous boot.
	if j.bootID != "" && r.text("_BOOT_ID") == j.bootID && r.text("_PID") == strconv.Itoa(os.Getpid()) {
		return event.Event{}, false, nil
	}
	unit := r.text("_SYSTEMD_UNIT")
	// Service state transitions are emitted by the system manager, not the
	// service itself. Accept its UNIT field only from trusted PID 1; never
	// treat a user unit's similarly named message as a system service event.
	if r.text("_PID") == "1" && r.text("UNIT") != "" {
		unit = r.text("UNIT")
	}
	allowed := false
	for _, configured := range j.cfg.Units {
		allowed = allowed || unit == configured
	}
	priority, err := strconv.Atoi(r.text("PRIORITY"))
	threshold, valid := config.JournalPriority(j.cfg.Priority)
	if !allowed || !valid || err != nil || priority < 0 || priority > threshold {
		return event.Event{}, false, nil
	}
	micros, err := strconv.ParseInt(r.text("__REALTIME_TIMESTAMP"), 10, 64)
	if err != nil || micros <= 0 {
		return event.Event{}, false, fmt.Errorf("selected journal record has an invalid timestamp")
	}
	message := r.text("MESSAGE")
	data := map[string]any{"unit": unit, "priority": priority, "message": message}
	// Default journalctl JSON omits large fields and represents binary ones
	// as arrays. Record the omission rather than pretending an empty message
	// was complete. Never request --all, which removes that upstream bound.
	if message == "" {
		data["message_omitted"] = true
	}
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		data["message"] = message
		data["message_truncated"] = true
	}
	return event.Event{
		ID: event.NewID("evt_"), Type: j.cfg.EventType, Source: j.cfg.Name,
		Subject: unit, Timestamp: time.UnixMicro(micros).UTC(), Data: data,
		Metadata: map[string]any{"source_event_id": cursor},
	}, true, nil
}

// read bounds both child lifetime and output. No shell, pager, follow mode,
// or user-supplied command flags are involved. Even stderr is private: it can
// contain paths or log material, so failures return fixed actionable errors.
func (j *Journal) read(ctx context.Context, fields string, args ...string) ([]journalRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	argv := append([]string{"--system", "--no-pager", "--output=json", "--output-fields=" + fields}, args...)
	cmd := exec.CommandContext(ctx, j.path, argv...)
	cmd.WaitDelay = time.Second
	out, stderr := journalOutput{limit: 4 << 20}, journalOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || stderr.buf.Len() != 0 {
		return nil, fmt.Errorf("journalctl failed or warned; verify system journal read access (often the systemd-journal or adm group), journal health, and support for --lines=+N; do not run mrmr as root")
	}
	dec := json.NewDecoder(&out.buf)
	var records []journalRecord
	for {
		var record journalRecord
		if err := dec.Decode(&record); err == io.EOF {
			return records, nil
		} else if err != nil || record == nil {
			return nil, fmt.Errorf("journalctl returned invalid JSON records")
		}
		records = append(records, record)
		if len(records) > journalBatchSize+1 {
			return nil, fmt.Errorf("journalctl exceeded the record limit")
		}
	}
}

// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy bypass
// Write and defeat the memory bound when exec copies the child's output.
type journalOutput struct {
	buf   bytes.Buffer
	limit int
}

func (b *journalOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		return 0, fmt.Errorf("journalctl output limit exceeded")
	}
	return b.buf.Write(p)
}
