package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/heath0xff/mrmr/internal/notify"
)

// Enrollment changes credentials, not policy. Only this authenticated control
// route accepts a URL; configuration, audit, and response data keep references.
func (m *manager) saveDiscord(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Expected string `json:"expected"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decodeControl(w, r, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fmt.Errorf("Confirm that saving changes the destination for every monitor using this connection.")
	}
	if err := notify.ValidateDiscordConnection(req.Name, req.URL); err != nil {
		return err
	}
	if err := m.audit("discord-save", req.Name, "requested"); err != nil {
		return fmt.Errorf("Cannot record credential-change intent; connection was not changed.")
	}
	id, err := m.discord.Save(req.Name, req.URL, req.Expected)
	result := "not saved"
	if id != "" {
		result = "saved " + id
	}
	if auditErr := m.audit("discord-save", req.Name, result); auditErr != nil {
		return fmt.Errorf("Cannot record credential-change result. Reload connection status before retrying.")
	}
	if errors.Is(err, notify.ErrDiscordConflict) {
		http.Error(w, err.Error(), http.StatusConflict)
		return nil
	}
	if err != nil {
		return err
	}
	return controlJSON(w, map[string]string{"name": req.Name, "revision": id, "notice": "Discord connection saved. No message sent. Shadow rules and active configuration are unchanged."})
}
