package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func recoverLocalPassword(ctx context.Context, args []string, input *os.File, output io.Writer, lookup func(string) (string, bool)) error {
	flags := flag.NewFlagSet("recover-local-password", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	target := flags.String("user", "", "existing local User UUID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("recovery accepts only --user UUID; provide the new password on stdin")
	}
	user, err := uuid.Parse(*target)
	if err != nil || user == uuid.Nil {
		return errors.New("recovery requires an existing local User UUID")
	}
	info, err := input.Stat()
	if err != nil {
		return errors.New("cannot read recovery input")
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("redirect or pipe the new password on stdin; terminal input is refused to prevent echo")
	}
	dsn, err := config.LoadRecoveryDatabase(lookup)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	database, err := storage.OpenDatabase(ctx, dsn)
	if err != nil {
		return errors.New("cannot open deployment database")
	}
	defer database.Close()
	if err = identity.RecoverLocalPassword(ctx, database, user, input); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Local password recovered; all User sessions were revoked. Bootstrap remains retired.")
	return err
}
