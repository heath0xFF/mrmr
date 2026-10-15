package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heath0xff/mrmr/internal/superhuman"
)

func authCommand(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAuth(ctx, args, os.Stdout)
}

// Authentication does not load a flow or open its database. This lets a user
// establish and check the connection before giving a source any mail access.
func runAuth(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[1] != "superhuman" || (args[0] != "login" && args[0] != "check") {
		return errors.New("usage: mrmr auth <login|check> superhuman [-state-dir /absolute/private/path] [-callback-port 18742 (login only)]")
	}
	fs := flag.NewFlagSet("auth "+args[0]+" superhuman", flag.ContinueOnError)
	dir := fs.String("state-dir", "", "private credential directory (default: user config directory/mrmr/superhuman)")
	port := 18742
	if args[0] == "login" {
		fs.IntVar(&port, "callback-port", port, "loopback callback port; forward this port when signing in over SSH")
	}
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected auth arguments")
	}
	if *dir == "" {
		var err error
		*dir, err = superhuman.DefaultDir()
		if err != nil {
			return err
		}
	}
	if args[0] == "login" {
		if err := superhuman.Login(ctx, *dir, port, func(url string) error {
			_, err := fmt.Fprintf(out, "Open this private sign-in link in your browser (expires in 5 minutes):\n%s\n", url)
			return err
		}); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Signed in to Superhuman. Credentials saved privately by mrmr; no email read or polling started.")
		return err
	}
	// A check is bounded and non-interactive, including token refresh. It is
	// deliberately not a hidden way to sign in or obtain broader permissions.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	count, err := superhuman.Check(ctx, *dir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Superhuman connected: %d linked account(s); required read tools available. No email bodies fetched.\n", count)
	return err
}
