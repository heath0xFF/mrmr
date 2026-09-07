package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// inspect answers "why did this happen?" for one event after the fact. The
// runtime already returns the trace to whoever posted the event, but that
// answer is gone the moment the caller drops the response; this reads the
// durable copy, together with the decisions, executions, and human label
// stored alongside it.
//
// Output is one indented JSON object rather than a rendered report: it is
// the same shape the ingest response has, so the same eyes and the same
// jq expressions work on both.
func inspect(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mrmr inspect EVENT_ID [-config mrmr.yaml]")
	}
	eventID := args[0]
	fs := flag.NewFlagSet("mrmr inspect", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	// Flags follow the event id, matching `mrmr label`.
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	_, db, err := openConfigDB(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ins, err := db.Inspect(eventID)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(ins)
}
