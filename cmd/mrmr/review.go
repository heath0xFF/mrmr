package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/storage"
)

func events(args []string) error {
	fs := flag.NewFlagSet("mrmr events", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	unlabeled := fs.Bool("unlabeled", false, "show only events without a human label")
	limit := fs.Int("limit", 20, "maximum events to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return fmt.Errorf("events limit must be between 1 and 1000")
	}

	_, db, err := openConfigDB(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.ListEvents(*limit, *unlabeled)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("write events: %w", err)
		}
	}
	return nil
}

func label(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mrmr label EVENT_ID -category CATEGORY -requires-action=true|false -outcome ignore|notify")
	}
	eventID := args[0]
	fs := flag.NewFlagSet("mrmr label", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	category := fs.String("category", "", "expected category")
	requiresActionText := fs.String("requires-action", "", "whether action is required: true or false")
	outcome := fs.String("outcome", "", "expected outcome: ignore or notify")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *category == "" || *requiresActionText == "" || *outcome == "" {
		return fmt.Errorf("category, requires-action, and outcome are required")
	}
	requiresAction, err := strconv.ParseBool(*requiresActionText)
	if err != nil {
		return fmt.Errorf("requires-action must be true or false")
	}

	cfg, db, err := openConfigDB(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := validateLabelCategory(cfg, *category); err != nil {
		return err
	}
	stored, err := db.LabelEvent(eventID, *category, requiresAction, *outcome)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(stored)
}

func dataset(args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return fmt.Errorf("usage: mrmr dataset export [-config mrmr.yaml] [-output path|-]")
	}
	fs := flag.NewFlagSet("mrmr dataset export", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	output := fs.String("output", "-", "output JSONL path, or - for stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	_, db, err := openConfigDB(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.LabeledEvents()
	if err != nil {
		return err
	}

	var out io.Writer = os.Stdout
	var file *os.File
	if *output != "-" {
		file, err = os.OpenFile(*output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("create dataset %s: %w", *output, err)
		}
		defer file.Close()
		out = file
	}
	if err := writeDataset(out, rows); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "exported %d labeled events\n", len(rows))
	return nil
}

func openConfigDB(path string) (*config.Config, *storage.DB, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	db, err := storage.Open(cfg.DB.Path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, db, nil
}

func validateLabelCategory(cfg *config.Config, category string) error {
	field, ok := cfg.Interpret.Schema["category"]
	if !ok || field.Type != "string" {
		return fmt.Errorf("labeling requires string interpret.schema field %q", "category")
	}
	if len(field.Enum) == 0 {
		return nil
	}
	for _, allowed := range field.Enum {
		if fmt.Sprint(allowed) == category {
			return nil
		}
	}
	return fmt.Errorf("category %q is not allowed by interpret.schema", category)
}

func writeDataset(w io.Writer, rows []storage.LabeledEvent) error {
	enc := json.NewEncoder(w)
	for _, row := range rows {
		tc := evalCase{Name: row.Event.ID, Event: row.Event}
		tc.Expected.Category = row.Label.Category
		tc.Expected.RequiresAction = new(bool)
		*tc.Expected.RequiresAction = row.Label.RequiresAction
		tc.Expected.Outcome = row.Label.ExpectedOutcome
		if err := enc.Encode(tc); err != nil {
			return fmt.Errorf("write dataset: %w", err)
		}
	}
	return nil
}
