package llmgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/station/platform"
)

const (
	ScopeManagementRead        = "llm-gateway.management.read"
	ScopeManagementWrite       = "llm-gateway.management.write"
	ScopeManagementActivate    = "llm-gateway.management.activate"
	ScopeManagementCredentials = "llm-gateway.management.credentials"
	managementPrefix           = "/v1/management"
)

// WithManagement enables the management routes for an already loaded database
// snapshot. The command's -gateway-db loader supplies the actual running ID;
// callers must not infer it from a later read of the selected revision.
func (p *Plugin) WithManagement(path string, runningRevision int64) *Plugin {
	out := *p
	out.managementPath, out.runningRevision = path, runningRevision
	return &out
}

type managementAPI struct {
	store   *ManagementStore
	running int64
	logger  *slog.Logger
	reg     *platform.PluginRegistry
}

func (p *Plugin) registerManagement(reg *platform.PluginRegistry, store *ManagementStore) error {
	m := &managementAPI{store: store, running: p.runningRevision, logger: reg.Logger(), reg: reg}
	routes := []platform.Route{
		{Method: "GET", Path: "/status", Scope: ScopeManagementRead, Handler: http.HandlerFunc(m.status)},
		{Method: "GET", Path: "/revisions", Scope: ScopeManagementRead, Handler: http.HandlerFunc(m.history)},
		{Method: "GET", Path: "/activations", Scope: ScopeManagementRead, Handler: http.HandlerFunc(m.activations)},
		{Method: "POST", Path: "/validate", Scope: ScopeManagementWrite, Handler: http.HandlerFunc(m.validate)},
		{Method: "POST", Path: "/activate", Scope: ScopeManagementActivate, Handler: http.HandlerFunc(m.activate)},
		{Method: "GET", Path: "/clients", Scope: ScopeManagementRead, Handler: http.HandlerFunc(m.clients)},
		{Method: "POST", Path: "/clients", Scope: ScopeManagementCredentials, Handler: http.HandlerFunc(m.editClient)},
		{Method: "PUT", Path: "/clients", Scope: ScopeManagementCredentials, Handler: http.HandlerFunc(m.editClient)},
		{Method: "DELETE", Path: "/clients", Scope: ScopeManagementCredentials, Handler: http.HandlerFunc(m.editClient)},
		{Method: "POST", Path: "/clients/rotate", Scope: ScopeManagementCredentials, Handler: http.HandlerFunc(m.editClient)},
	}
	for _, resource := range []string{"config", "providers", "models", "policy", "budgets"} {
		routes = append(routes,
			platform.Route{Method: "GET", Path: "/" + resource, Scope: ScopeManagementRead, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.readConfig(w, r, resource) })},
			platform.Route{Method: "PUT", Path: "/" + resource, Scope: ScopeManagementWrite, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.editConfig(w, r, resource) })})
	}
	for _, rt := range routes {
		rt.Path = managementPrefix + rt.Path
		rt.MaxBodyBytes = maxManagementDocument
		rt.Handler = m.authorize(rt.Scope, rt.Handler)
		if err := reg.Mount(rt); err != nil {
			return err
		}
	}
	return nil
}

// Defense in depth: management always needs a verified bearer principal and
// scope even if this plugin is embedded in a permissive/off-mode host.
func (m *managementAPI) authorize(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		p, ok := auth.FromContext(r.Context())
		_, state := auth.ParseBearer(r.Header.Values("Authorization"))
		if !ok || !p.Verified || state != "" {
			m.problem(w, r, 401, "unauthenticated", "a verified bearer credential is required")
			return
		}
		if !p.HasScope(scope) {
			m.problem(w, r, 403, "forbidden", "management scope is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *managementAPI) problem(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": platform.RequestID(r)}})
}

func (m *managementAPI) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRevisionConflict):
		m.problem(w, r, 412, "revision_conflict", "revision changed; read again before retrying")
	case errors.Is(err, sql.ErrNoRows):
		m.problem(w, r, 404, "not_found", "revision does not exist")
	default:
		m.logger.Error("gateway.management.store_failed", "request_id", platform.RequestID(r))
		m.problem(w, r, 503, "store_unavailable", "gateway state is unavailable")
	}
}

func revisionETag(id int64) string { return strconv.Quote(strconv.FormatInt(id, 10)) }

// One strong numeric ETag is required. Wildcards, weak tags and lists could
// erase the stale-editor protection and are deliberately unsupported.
func (m *managementAPI) expected(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		m.problem(w, r, 428, "precondition_required", "If-Match must contain the revision ETag")
		return 0, false
	}
	if len(values) != 1 {
		m.problem(w, r, 400, "invalid_precondition", "expected one strong revision ETag")
		return 0, false
	}
	value := strings.TrimSpace(values[0])
	n, err := strconv.ParseInt(strings.Trim(value, "\""), 10, 64)
	if err != nil || n < 0 || value != revisionETag(n) {
		m.problem(w, r, 400, "invalid_precondition", "expected one strong revision ETag")
		return 0, false
	}
	return n, true
}

func strictJSON(raw []byte, dst any) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("a JSON value is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func (m *managementAPI) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		m.problem(w, r, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManagementDocument))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		m.problem(w, r, 413, "body_too_large", "management body exceeds 4 MiB")
		return false
	}
	if err != nil || strictJSON(raw, dst) != nil {
		m.problem(w, r, 400, "invalid_json", "expected one typed JSON value with no unknown fields")
		return false
	}
	return true
}

func (m *managementAPI) status(w http.ResponseWriter, r *http.Request) {
	st, err := m.store.Status(r.Context())
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	w.Header().Set("ETag", revisionETag(st.ActiveRevision))
	writeJSON(w, 200, map[string]any{"schema_version": ManagementDocumentVersion, "latest_revision": st.LatestRevision, "selected_revision": st.ActiveRevision, "running_revision": m.running, "restart_required": st.ActiveRevision != m.running, "staged_changes": st.LatestRevision != st.ActiveRevision, "apply_mode": "service_reload"})
}

func (m *managementAPI) readRevision(w http.ResponseWriter, r *http.Request) (ManagementRevision, bool) {
	var id int64
	if value := r.URL.Query().Get("revision"); value != "" {
		var err error
		id, err = strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			m.problem(w, r, 400, "invalid_revision", "revision must be a positive integer")
			return ManagementRevision{}, false
		}
	} else {
		st, err := m.store.Status(r.Context())
		if err != nil {
			m.storeError(w, r, err)
			return ManagementRevision{}, false
		}
		id = st.LatestRevision
	}
	rev, err := m.store.Revision(r.Context(), id)
	if err != nil {
		m.storeError(w, r, err)
		return rev, false
	}
	w.Header().Set("ETag", revisionETag(rev.ID))
	return rev, true
}

func (m *managementAPI) editRevision(w http.ResponseWriter, r *http.Request) (ManagementRevision, bool) {
	expected, ok := m.expected(w, r)
	if !ok {
		return ManagementRevision{}, false
	}
	st, err := m.store.Status(r.Context())
	if err != nil {
		m.storeError(w, r, err)
		return ManagementRevision{}, false
	}
	if expected != st.LatestRevision {
		m.storeError(w, r, ErrRevisionConflict)
		return ManagementRevision{}, false
	}
	rev, err := m.store.Revision(r.Context(), expected)
	if err != nil {
		m.storeError(w, r, err)
		return rev, false
	}
	return rev, true
}

type gatewayEdit struct {
	SchemaVersion int     `json:"schema_version"`
	Gateway       *Config `json:"gateway"`
}

func (m *managementAPI) readConfig(w http.ResponseWriter, r *http.Request, resource string) {
	rev, ok := m.readRevision(w, r)
	if !ok {
		return
	}
	// Resource reads must round-trip through PUT, which requires an explicit
	// array rather than null when clearing a collection.
	gateway := rev.Document.Gateway
	if gateway.Providers == nil {
		gateway.Providers = []ProviderConfig{}
	}
	if gateway.Models == nil {
		gateway.Models = []ModelConfig{}
	}
	if gateway.Budgets == nil {
		gateway.Budgets = []BudgetConfig{}
	}
	result := map[string]any{"revision": rev.ID, "schema_version": ManagementDocumentVersion}
	switch resource {
	case "config":
		result["gateway"] = gateway
	case "providers":
		result[resource] = gateway.Providers
	case "models":
		result[resource] = gateway.Models
	case "policy":
		result[resource] = gateway.Policy
	case "budgets":
		result[resource] = gateway.Budgets
	}
	writeJSON(w, 200, result)
}

func (m *managementAPI) changeConfig(w http.ResponseWriter, r *http.Request, doc *ManagementDocument, resource string) bool {
	if resource == "config" {
		var body gatewayEdit
		if !m.decode(w, r, &body) {
			return false
		}
		if body.SchemaVersion != ManagementDocumentVersion || body.Gateway == nil {
			m.problem(w, r, 422, "invalid_configuration", "schema_version 1 and gateway are required")
			return false
		}
		doc.Gateway = *body.Gateway
	} else {
		// Named envelopes prevent an empty/misspelled body from silently
		// clearing a collection. An explicit [] does clear models/budgets etc.
		var body map[string]json.RawMessage
		if !m.decode(w, r, &body) {
			return false
		}
		raw, ok := body[resource]
		if !ok || len(body) != 1 {
			m.problem(w, r, 400, "invalid_json", "expected only the named resource field")
			return false
		}
		var dst any
		switch resource {
		case "providers":
			dst = &doc.Gateway.Providers
		case "models":
			dst = &doc.Gateway.Models
		case "policy":
			dst = &doc.Gateway.Policy
		case "budgets":
			dst = &doc.Gateway.Budgets
		}
		if err := strictJSON(raw, dst); err != nil {
			m.problem(w, r, 400, "invalid_json", "invalid resource value or unknown field")
			return false
		}
	}
	return m.validDocument(w, r, *doc)
}

func (m *managementAPI) validDocument(w http.ResponseWriter, r *http.Request, doc ManagementDocument) bool {
	if err := doc.Validate(); err != nil {
		m.problem(w, r, 422, "invalid_configuration", err.Error())
		return false
	}
	return true
}

func (m *managementAPI) editConfig(w http.ResponseWriter, r *http.Request, resource string) {
	rev, ok := m.editRevision(w, r)
	if !ok {
		return
	}
	if !m.changeConfig(w, r, rev.Document, resource) {
		return
	}
	m.save(w, r, rev, "update_"+resource, nil)
}

func (m *managementAPI) validate(w http.ResponseWriter, r *http.Request) {
	doc := ManagementDocument{SchemaVersion: ManagementDocumentVersion}
	if !m.changeConfig(w, r, &doc, "config") {
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true, "schema_version": ManagementDocumentVersion, "validation": "configuration_only", "runtime_checked": false})
}

func (m *managementAPI) save(w http.ResponseWriter, r *http.Request, rev ManagementRevision, action string, extra map[string]any) {
	p, _ := auth.FromContext(r.Context())
	saved, err := m.store.Save(r.Context(), *rev.Document, rev.ID, p.ID)
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	m.logger.Info("gateway.management.staged", "request_id", platform.RequestID(r), "principal", p.ID, "action", action, "revision", saved.ID)
	if extra == nil {
		extra = map[string]any{}
	}
	extra["revision"], extra["state"] = saved.ID, "staged"
	w.Header().Set("ETag", revisionETag(saved.ID))
	writeJSON(w, 201, extra)
}

func page(r *http.Request) (int64, int, error) {
	before := int64(0)
	limit := 100
	var err error
	if value := r.URL.Query().Get("before"); value != "" {
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			return 0, 0, errors.New("invalid before")
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, errors.New("invalid limit")
		}
	}
	return before, limit, nil
}

func (m *managementAPI) history(w http.ResponseWriter, r *http.Request) {
	before, limit, err := page(r)
	if err != nil {
		m.problem(w, r, 400, "invalid_page", "before must be positive and limit must be 1–100")
		return
	}
	items, err := m.store.History(r.Context(), before, limit)
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	var next int64
	if len(items) == limit {
		next = items[len(items)-1].ID
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_before": next})
}

func (m *managementAPI) activations(w http.ResponseWriter, r *http.Request) {
	before, limit, err := page(r)
	if err != nil {
		m.problem(w, r, 400, "invalid_page", "before must be positive and limit must be 1–100")
		return
	}
	items, err := m.store.Activations(r.Context(), before, limit)
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	var next int64
	if len(items) == limit {
		next = items[len(items)-1].ID
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_before": next})
}

func (m *managementAPI) activate(w http.ResponseWriter, r *http.Request) {
	expected, ok := m.expected(w, r)
	if !ok {
		return
	}
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !m.decode(w, r, &body) {
		return
	}
	if body.Revision <= 0 {
		m.problem(w, r, 422, "invalid_revision", "revision must be positive")
		return
	}
	current, err := m.store.Active(r.Context())
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	if current.ID != expected {
		m.storeError(w, r, ErrRevisionConflict)
		return
	}
	target, err := m.store.Revision(r.Context(), body.Revision)
	if err != nil {
		m.storeError(w, r, err)
		return
	}
	p, _ := auth.FromContext(r.Context())
	if !credentialChangesAllowed(p, current.Document.Clients, target.Document.Clients) {
		m.problem(w, r, 403, "credential_authority_required", "activation changes credentials outside this caller's authority")
		return
	}
	if err := m.store.Activate(r.Context(), target.ID, expected, p.ID); err != nil {
		m.storeError(w, r, err)
		return
	}
	m.logger.Info("gateway.management.selected", "request_id", platform.RequestID(r), "principal", p.ID, "previous_revision", expected, "revision", target.ID)
	w.Header().Set("ETag", revisionETag(target.ID))
	writeJSON(w, 202, map[string]any{"selected_revision": target.ID, "running_revision": m.running, "restart_required": target.ID != m.running, "apply_mode": "service_reload"})
}

type managedClient struct {
	ID     string   `json:"id"`
	Scopes []string `json:"scopes"`
}

func (m *managementAPI) clients(w http.ResponseWriter, r *http.Request) {
	rev, ok := m.readRevision(w, r)
	if !ok {
		return
	}
	items := make([]managedClient, 0, len(rev.Document.Clients))
	for _, c := range rev.Document.Clients {
		items = append(items, managedClient{ID: c.ID, Scopes: c.Scopes})
	}
	writeJSON(w, 200, map[string]any{"revision": rev.ID, "clients": items})
}

func holdsScopes(p auth.Principal, scopes []string) bool {
	for _, scope := range scopes {
		if !p.HasScope(scope) {
			return false
		}
	}
	return true
}

// Selecting an old document must not be a back door around credential scopes.
func credentialChangesAllowed(p auth.Principal, before, after []platform.KeyConfig) bool {
	a, b := map[string]platform.KeyConfig{}, map[string]platform.KeyConfig{}
	for _, c := range before {
		a[c.ID] = c
	}
	for _, c := range after {
		b[c.ID] = c
	}
	for id, old := range a {
		if !reflect.DeepEqual(old, b[id]) {
			if !p.HasScope(ScopeManagementCredentials) || !holdsScopes(p, old.Scopes) || !holdsScopes(p, b[id].Scopes) {
				return false
			}
		}
	}
	for id, next := range b {
		if _, ok := a[id]; !ok {
			if !p.HasScope(ScopeManagementCredentials) || !holdsScopes(p, next.Scopes) {
				return false
			}
		}
	}
	return true
}

func (m *managementAPI) editClient(w http.ResponseWriter, r *http.Request) {
	rev, ok := m.editRevision(w, r)
	if !ok {
		return
	}
	var body managedClient
	rotate := strings.HasSuffix(r.URL.Path, "/rotate")
	if r.Method == "DELETE" {
		body.ID = r.URL.Query().Get("id")
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			m.problem(w, r, 400, "invalid_request", "DELETE uses the id query parameter and no body")
			return
		}
	} else if rotate {
		var input struct {
			ID string `json:"id"`
		}
		if !m.decode(w, r, &input) {
			return
		}
		body.ID = input.ID
	} else if !m.decode(w, r, &body) {
		return
	}
	if body.ID == "" {
		m.problem(w, r, 422, "invalid_client", "client id is required")
		return
	}
	pos := -1
	for i, c := range rev.Document.Clients {
		if c.ID == body.ID {
			pos = i
			break
		}
	}
	create := r.Method == "POST" && !rotate
	if create && pos >= 0 {
		m.problem(w, r, 409, "client_exists", "client already exists; rotate it explicitly")
		return
	}
	if !create && pos < 0 {
		m.problem(w, r, 404, "not_found", "client does not exist")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if pos >= 0 && !holdsScopes(p, rev.Document.Clients[pos].Scopes) {
		m.problem(w, r, 403, "forbidden", "client has scopes outside this caller's authority")
		return
	}
	if rotate {
		body.Scopes = rev.Document.Clients[pos].Scopes
	}
	if r.Method != "DELETE" && !holdsScopes(p, body.Scopes) {
		m.problem(w, r, 403, "forbidden", "cannot grant scopes this caller does not hold")
		return
	}
	extra := map[string]any{"client": body}
	action := "update_client"
	switch {
	case create || rotate:
		var entropy [32]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			m.problem(w, r, 503, "credential_unavailable", "cannot generate credential")
			return
		}
		token := "stn_" + hex.EncodeToString(entropy[:])
		hash := sha256.Sum256([]byte(token))
		key := platform.KeyConfig{ID: body.ID, SHA256: hex.EncodeToString(hash[:]), Scopes: body.Scopes}
		if create {
			rev.Document.Clients = append(rev.Document.Clients, key)
			action = "issue_client"
		} else {
			rev.Document.Clients[pos] = key
			action = "rotate_client"
		}
		extra["token"] = token
		m.reg.RedactScope().Add("issued-client-token", token)
	case r.Method == "DELETE":
		rev.Document.Clients = append(rev.Document.Clients[:pos], rev.Document.Clients[pos+1:]...)
		action = "revoke_client"
	default:
		rev.Document.Clients[pos].Scopes = body.Scopes
	}
	if !m.validDocument(w, r, *rev.Document) {
		return
	}
	m.save(w, r, rev, action, extra)
}
