package identity

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// RecoverLocalPassword accepts bounded stdin and uses deployment database authority.
func RecoverLocalPassword(ctx context.Context, database *storage.Database, user uuid.UUID, input io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(input, maxPasswordBytes+3))
	if err != nil {
		return errors.New("cannot read recovery password")
	}
	defer clear(raw)
	password := bytes.TrimSuffix(raw, []byte("\n"))
	password = bytes.TrimSuffix(password, []byte("\r"))
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return errors.New("recovery password must be between 12 and 1024 bytes")
	}
	encoded, err := hashPassword(string(password))
	if err != nil {
		return errors.New("cannot prepare recovery password")
	}
	if err = database.RecoverLocalPassword(ctx, user, encoded); err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) {
			return errors.New("recovery requires an existing enabled local User")
		}
		return errors.New("password recovery failed; no credential change was confirmed")
	}
	return nil
}
