package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// initKeys registers every configured principal, then replays the saved runtime keys on
// top of them.
func (s *Server) initKeys() error {
	taken := map[string]bool{"default": true}
	for _, c := range s.clients {
		taken[c.name] = true
	}
	users, err := buildUsers(s.cfg, taken, s.now())
	if err != nil {
		return err
	}
	s.users, s.userHash, s.throttle = users, map[string]string{}, newThrottle()
	if len(users) > 0 {
		s.dummyHash = newDummyHash(s.cfg)
	}
	for _, u := range s.cfg.Users {
		s.userHash[u.Name] = u.PasswordHash
	}
	s.keys = newKeyStore(s.cfg)
	if s.cfg.Admin.SelfServiceKeys {
		switch {
		case s.cfg.Admin.StateFile == "":
			return fmt.Errorf("[admin] self_service_keys needs state_file: minted keys must survive a restart")
		case !s.HasClientKeys():
			return fmt.Errorf("[admin] self_service_keys needs at least one configured key or user to start from")
		}
	}
	for _, c := range s.clients {
		if s.keys.principals[c.name] == nil {
			s.keys.principals[c.name] = c
		}
	}
	for n, u := range users {
		s.keys.principals[n] = u
	}
	return s.keys.load(s.now(), s.router.knownName)
}

// strictJSON decodes a request body, refusing unknown fields so a misspelt one is an
// error rather than a silent no-op.
func strictJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	return nil
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedStrings(in []string) []string {
	sort.Strings(in)
	return in
}
