package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// profilesView is the body of GET /v1/profiles and of a successful profile edit. Every
// client sees the profiles it may use, with the model each points at (the model name is
// deliberately not treated as a secret). An admin also sees sources, the version, the
// aliases to choose from, the audit trail and any stale overrides.
func (s *Server) profilesView(r *http.Request, c *client) map[string]any {
	rc := s.router.rc
	ps := rc.snapshot()
	status := map[string]string{}
	for _, m := range s.statuses(r.Context()) {
		status[m.ID] = m.Status
	}
	list := []map[string]any{}
	for _, name := range ps.names() {
		if !c.visible(name) {
			continue
		}
		item := map[string]any{"name": name, "target": ps.target[name], "status": status[ps.target[name]]}
		if c.admin {
			item["source"] = ps.source[name]
			if b, inConfig := rc.baseProfiles[name]; inConfig && b != ps.target[name] {
				item["config_target"] = b
			}
		}
		list = append(list, item)
	}
	out := map[string]any{"profiles": list, "editable": rc.editProfiles, "persisted": rc.statePath != ""}
	if c.admin {
		out["version"], out["aliases"], out["changes"], out["stale"] = ps.version, s.router.ModelNames(), rc.recentChanges(), rc.staleOverrides()
	}
	return out
}

// routesView is the body of GET /v1/routes and of a successful route edit. Plans are
// shown as authored (profile names unresolved). An admin also gets the editing context.
func (s *Server) routesView(c *client) map[string]any {
	rc := s.router.rc
	ps := rc.snapshot()
	shown := map[string]ignatius.Plan{}
	for name, p := range ps.route {
		if c.visible(name) {
			shown[name] = p
		}
	}
	def := ""
	if c.visible(s.cfg.DefaultRoute) {
		def = s.cfg.DefaultRoute
	}
	grammar := ""
	if c.visible("inline") && s.router.AllowInline {
		grammar = ignatius.InlineGrammar
	}
	out := map[string]any{"default": def, "routes": shown, "inline_grammar": grammar}
	if c.admin {
		sources, configSummary := map[string]string{}, map[string]string{}
		for name := range shown {
			sources[name] = ps.routeSource[name]
			if b, inConfig := rc.baseRoutes[name]; inConfig && ps.routeSource[name] == "override" {
				configSummary[name] = planSummary(b)
			}
		}
		out["editable"], out["persisted"], out["version"] = rc.editRoutes, rc.statePath != "", ps.version
		out["sources"], out["config_summaries"] = sources, configSummary
		out["choices"] = map[string]any{"aliases": s.router.ModelNames(), "profiles": ps.names()}
		out["changes"], out["stale"] = rc.recentChanges(), rc.staleOverrides()
	}
	return out
}

func (s *Server) handleProfilesGet(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.profilesView(r, c))
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.routesView(c))
}

type editBody struct {
	Target          string         `json:"target"`
	Plan            *ignatius.Plan `json:"plan"`
	ExpectedVersion *uint64        `json:"expected_version"`
}

// editConfig runs an edit as an authenticated admin and maps its errors to HTTP.
func (s *Server) editConfig(w http.ResponseWriter, r *http.Request, wantBody bool,
	view func(*http.Request, *client) map[string]any,
	do func(c *client, name string, in editBody) (ConfigChange, bool, error)) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if !c.admin {
		writeErr(w, http.StatusForbidden, "admin_required", "editing needs an admin key", nil)
		return
	}
	var in editBody
	if wantBody || r.ContentLength > 0 {
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields() // a misspelt field must be an error, not a silent no-op
			if err := dec.Decode(&in); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_json", "could not read the body: "+err.Error(), nil)
				return
			}
		}
	}
	// A DELETE's version travels in the URL: proxies and CDNs commonly strip a DELETE's body,
	// which would silently switch the conflict check off. The body is still accepted.
	if in.ExpectedVersion == nil {
		if qv := r.URL.Query().Get("expected_version"); qv != "" {
			n, err := strconv.ParseUint(qv, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_query", "expected_version must be a whole number", nil)
				return
			}
			in.ExpectedVersion = &n
		}
	}
	change, changed, err := do(c, r.PathValue("name"), in)
	var ee *EditError
	switch {
	case errors.Is(err, ErrEditingDisabled):
		writeErr(w, http.StatusForbidden, "editing_disabled", err.Error(), nil)
		return
	case errors.Is(err, ErrVersionConflict):
		writeErr(w, http.StatusConflict, "version_conflict", err.Error(),
			map[string]any{"current_version": s.router.rc.snapshot().version})
		return
	case errors.Is(err, ErrNotRemovable):
		writeErr(w, http.StatusConflict, "not_removable", err.Error(), nil)
		return
	case errors.As(err, &ee):
		writeErr(w, http.StatusUnprocessableEntity, "invalid_edit", ee.Msg, nil)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error(), nil)
		return
	}
	if changed { // the durable audit trail; a no-op edit is not a change
		s.log.InfoContext(r.Context(), "config_changed", "actor", c.name, "kind", change.Kind, "op", change.Op,
			"name", change.Name, "from", change.From, "to", change.To, "version", change.Version)
	}
	v := view(r, c)
	v["change"], v["changed"] = change, changed
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleProfilePut(w http.ResponseWriter, r *http.Request) {
	s.editConfig(w, r, true, s.profilesView, func(c *client, name string, in editBody) (ConfigChange, bool, error) {
		return s.router.rc.setProfile(c.name, name, in.Target, in.ExpectedVersion)
	})
}

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	s.editConfig(w, r, false, s.profilesView, func(c *client, name string, in editBody) (ConfigChange, bool, error) {
		return s.router.rc.removeProfile(c.name, name, in.ExpectedVersion)
	})
}

func (s *Server) handleRoutePut(w http.ResponseWriter, r *http.Request) {
	routes := func(_ *http.Request, c *client) map[string]any { return s.routesView(c) }
	s.editConfig(w, r, true, routes, func(c *client, name string, in editBody) (ConfigChange, bool, error) {
		if in.Plan == nil {
			return ConfigChange{}, false, bad(`a route needs a "plan", e.g. {"plan": {"mode": "cascade", "tiers": [...]}}`)
		}
		return s.router.rc.setRoute(c.name, name, *in.Plan, in.ExpectedVersion)
	})
}

func (s *Server) handleRouteDelete(w http.ResponseWriter, r *http.Request) {
	routes := func(_ *http.Request, c *client) map[string]any { return s.routesView(c) }
	s.editConfig(w, r, false, routes, func(c *client, name string, in editBody) (ConfigChange, bool, error) {
		return s.router.rc.removeRoute(c.name, name, in.ExpectedVersion)
	})
}
