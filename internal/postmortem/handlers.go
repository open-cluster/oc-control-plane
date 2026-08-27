package postmortem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	requestTimeout  = 30 * time.Second
	maxRequestBytes = 64 << 10
)

type Handlers struct {
	Service Service
	Logger  *slog.Logger
}

func (h Handlers) Routes() authz.Table {
	const base = "/api/v1/organizations/{organization}/incidents/{incident}/postmortem"
	return authz.Table{
		authz.Privileged(http.MethodGet, base, authz.PostmortemRead, http.HandlerFunc(h.get)),
		authz.Privileged(http.MethodPost, base, authz.PostmortemWrite, http.HandlerFunc(h.generate)),
		authz.Privileged(http.MethodPost, base+"/regenerate", authz.PostmortemWrite,
			http.HandlerFunc(h.regenerate)),
		authz.Privileged(http.MethodPatch, base, authz.PostmortemWrite, http.HandlerFunc(h.correct)),
		authz.Privileged(http.MethodPost, base+"/review", authz.PostmortemWrite,
			http.HandlerFunc(h.review)),
	}
}

func (h Handlers) get(writer http.ResponseWriter, request *http.Request) {
	organization, incident, _, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	found, err := h.Service.Store.Postmortem(ctx, organization, incident)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, found)
}

func (h Handlers) generate(writer http.ResponseWriter, request *http.Request) {
	organization, incident, principal, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var human HumanInput
	if !decodeOptional(writer, request, &human) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	found, err := h.Service.Generate(ctx, principal, organization, incident, human)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, found)
}

func (h Handlers) regenerate(writer http.ResponseWriter, request *http.Request) {
	organization, incident, principal, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var human HumanInput
	if !decodeOptional(writer, request, &human) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	found, err := h.Service.Regenerate(ctx, principal, organization, incident, human)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, found)
}

func (h Handlers) correct(writer http.ResponseWriter, request *http.Request) {
	organization, incident, principal, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var corrections Corrections
	if !decodeRequired(writer, request, &corrections) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	found, err := h.Service.Correct(ctx, principal, organization, incident, corrections)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, found)
}

func (h Handlers) review(writer http.ResponseWriter, request *http.Request) {
	organization, incident, principal, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
	defer cancel()
	found, err := h.Service.Review(ctx, principal, organization, incident)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, found)
}

func (h Handlers) addressed(
	writer http.ResponseWriter,
	request *http.Request,
) (tenancy.Organization, uuid.UUID, authz.Principal, bool) {
	principal, ok := authz.Of(request)
	if !ok {
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return tenancy.Organization{}, uuid.Nil, authz.Principal{}, false
	}
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, uuid.Nil, authz.Principal{}, false
	}
	incidentID, err := uuid.Parse(request.PathValue("incident"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "incident is not an identity"})
		return tenancy.Organization{}, uuid.Nil, authz.Principal{}, false
	}
	return organization, incidentID, principal, true
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "postmortem not found"})
	case errors.Is(err, ErrNotEligible):
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	case errors.Is(err, ErrAlreadyExists), errors.Is(err, ErrAlreadyReviewed):
		writeJSON(writer, http.StatusConflict, errorView{Error: err.Error()})
	default:
		if h.Logger != nil {
			h.Logger.ErrorContext(request.Context(), "postmortem request failed",
				slog.String("error", err.Error()))
		}
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

type errorView struct {
	Error string `json:"error"`
}

func decodeOptional(writer http.ResponseWriter, request *http.Request, into any) bool {
	return decodeBody(writer, request, into, true)
}
func decodeRequired(writer http.ResponseWriter, request *http.Request, into any) bool {
	return decodeBody(writer, request, into, false)
}
func decodeBody(writer http.ResponseWriter, request *http.Request, into any, optional bool) bool {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return true
		}
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
