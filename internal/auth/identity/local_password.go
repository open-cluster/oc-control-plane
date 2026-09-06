package identity

import (
	"errors"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type localPasswordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (h Handlers) changeLocalPassword(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	var body localPasswordChangeRequest
	if !decode(writer, request, &body) {
		return
	}
	if body.CurrentPassword == "" || len(body.CurrentPassword) > maxPasswordBytes || len(body.NewPassword) < minPasswordBytes || len(body.NewPassword) > maxPasswordBytes {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "invalid password length"})
		return
	}
	select {
	case passwordCheckSlots <- struct{}{}:
		defer func() { <-passwordCheckSlots }()
	default:
		writeJSON(writer, http.StatusTooManyRequests, errorView{Error: "password check capacity reached"})
		return
	}
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	previous, err := h.Database.LocalPasswordHash(ctx, principal)
	if errors.Is(err, storage.ErrLocalCredentialUnknown) {
		writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	valid, _, err := verifyPassword(previous, body.CurrentPassword)
	if err != nil || !valid {
		writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
		return
	}
	replacement, err := hashPassword(body.NewPassword)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if err = h.Database.ChangeLocalPassword(ctx, principal, previous, replacement); err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) {
			writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
			return
		}
		h.fail(writer, request, err)
		return
	}
	session.Clear(writer)
	writer.WriteHeader(http.StatusNoContent)
}
