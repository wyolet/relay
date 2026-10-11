package scim

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/auth/directory"
)

// maxBodyBytes bounds a request body; a full push of a large group fits well inside it.
const maxBodyBytes = 4 << 20

// Config wires a Handler.
type Config struct {
	Directory *directory.Directory
	Tokens    TokenStore
	// DeleteDisables makes DELETE /Users/{id} disable the account instead of deleting it, keeping its history and bindings.
	DeleteDisables bool
	// BaseURL is the external URL the handler is mounted at, such as https://example.com/scim/v2. It fills meta.location and $ref; empty leaves them out.
	BaseURL string
	// OnWrite hears every write a client attempted, after it ran, with its error; nil drops them.
	OnWrite func(ctx context.Context, w Write)
	// Logger receives internal errors; nil is slog.Default().
	Logger *slog.Logger
}

// Write is one write a client attempted, for a product's audit log.
type Write struct {
	Client string
	// Kind is "user" or "group".
	Kind string
	// Op is "create", "update", "disable", "enable" or "delete".
	Op   string
	ID   string
	Name string
	// Fields are the SCIM attributes an update changed.
	Fields []string
	// Err is why the write failed; a refused reach for a row the client did not provision matches directory.ErrForeign.
	Err error
}

// Handler is the SCIM service provider. It serves paths relative to its mount point, so mount it under a prefix with http.StripPrefix.
type Handler struct {
	cfg Config
	log *slog.Logger
}

// New returns a handler over cfg; Directory and Tokens are required.
func New(cfg Config) (*Handler, error) {
	if cfg.Directory == nil || cfg.Tokens == nil {
		return nil, errors.New("scim: a directory and a token store are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, log: log}, nil
}

// request is one authenticated call.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	p      *directory.Provisioner
	client string
}

func (rq *request) ctx() context.Context { return rq.r.Context() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := h.authenticate(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	p, err := h.cfg.Directory.Provisioner(c.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), clientKey{}, c.Name))
	rq := &request{w: w, r: r, p: p, client: c.Name}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	rest := segs[1:]
	switch {
	case strings.EqualFold(segs[0], "Users"):
		err = h.serveUsers(rq, rest)
	case strings.EqualFold(segs[0], "Groups"):
		err = h.serveGroups(rq, rest)
	case strings.EqualFold(segs[0], "ServiceProviderConfig") && len(rest) == 0:
		err = h.serveDiscovery(rq, serviceProviderConfig())
	case strings.EqualFold(segs[0], "ResourceTypes"):
		err = h.serveCatalog(rq, rest, resourceTypes(h))
	case strings.EqualFold(segs[0], "Schemas"):
		err = h.serveCatalog(rq, rest, schemaDefinitions())
	case strings.EqualFold(segs[0], "Bulk"):
		err = errorf(http.StatusNotImplemented, "", "bulk operations are not supported")
	default:
		err = errorf(http.StatusNotFound, "", "no such endpoint")
	}
	if err != nil {
		h.fail(w, err)
	}
}

// fail answers err, logging the cause of a 500 that the response hides.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if toError(err).Status >= http.StatusInternalServerError {
		h.log.Error("scim: request failed", "err", err)
	}
	writeError(w, err)
}

func methodNotAllowed() error {
	return errorf(http.StatusMethodNotAllowed, "", "method not allowed")
}

func (rq *request) body() ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(rq.w, rq.r.Body, maxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, errorf(http.StatusRequestEntityTooLarge, typeTooMany, "the body is larger than %d bytes", maxBodyBytes)
		}
		return nil, badRequest(typeInvalidSyntax, "read the body: %s", err.Error())
	}
	return raw, nil
}

// report hands the write to OnWrite and returns its error.
func (h *Handler) report(rq *request, w Write) error {
	if h.cfg.OnWrite != nil {
		w.Client = rq.client
		h.cfg.OnWrite(rq.ctx(), w)
	}
	return w.Err
}

func (rq *request) reply(status int, v any) error {
	writeJSON(rq.w, status, v)
	return nil
}

func (rq *request) noContent() error {
	rq.w.WriteHeader(http.StatusNoContent)
	return nil
}
