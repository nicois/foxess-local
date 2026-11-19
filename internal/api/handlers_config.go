package api

import (
	"errors"
	"net/http"
	"os"
	"time"
)

// configView is the spec's Config (the GET /config body — the
// read-only server facts, no secrets).
type configView struct {
	RawHours     int  `json:"raw_hours"`
	BucketDays   int  `json:"bucket_days"`
	PublicAccess bool `json:"public_access"`
	// PublicAllPages: a tokenless session may open every page, not only
	// the dashboard ([access] public_all_pages).
	PublicAllPages bool   `json:"public_all_pages"`
	Version        string `json:"version"`
	ForwardMode    string `json:"forward_mode"`
	// IssueURL is where to file a problem report (absent when unknown).
	IssueURL string `json:"issue_url,omitempty"`
	// TimeZone and UTCOffsetS: the host's own zone (informational: the
	// schedule and the inverter's clock follow the site zone).
	TimeZone   string `json:"time_zone"`
	UTCOffsetS int    `json:"utc_offset_s"`
	// TimeZoneName is the host's IANA zone (e.g. Australia/Sydney); null
	// when it cannot be told.
	TimeZoneName *string `json:"time_zone_name"`
	// SiteTimeZone is the site's IANA zone (the inverter clock's and
	// schedule's); null until chosen.
	SiteTimeZone *string `json:"site_time_zone"`
	// InverterClockOffsetS: the inverter's clock minus the site's time at
	// the last read (positive: the inverter is ahead); null before a
	// read or while no site zone is chosen.
	InverterClockOffsetS *float64 `json:"inverter_clock_offset_s"`
}

// handleConfig is GET /config (spec getConfig).
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request, _ Token) {
	mode, _, err := s.fwd.Get()
	if err != nil {
		s.internal(w, err)
		return
	}
	zone, offset := s.clock().In(time.Local).Zone()
	view := configView{
		RawHours:       s.info.RawHours,
		BucketDays:     s.info.BucketDays,
		PublicAccess:   s.info.values().PublicReadOnly,
		PublicAllPages: s.info.values().PublicAllPages,
		Version:        s.info.Version,
		ForwardMode:    string(mode),
		IssueURL:       s.info.IssueURL,
		TimeZone:       time.Local.String() + " (" + zone + ")",
		UTCOffsetS:     offset,
		TimeZoneName:   ianaZone(hostZoneSources, os.Getenv("TZ")),
	}
	if z := s.siteZone(); z != nil {
		name := z.String()
		view.SiteTimeZone = &name
	}
	if d, ok := s.inverterClockOffset(); ok {
		view.InverterClockOffsetS = &d
	}
	writeJSON(w, http.StatusOK, view)
}

// tokenInfo is the spec's TokenInfo (no secrets).
type tokenInfo struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used"`
}

// tokenCreated is the spec's TokenCreated (the plaintext exactly once).
type tokenCreated struct {
	Token   string    `json:"token"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	Created time.Time `json:"created"`
}

// handleTokenList is GET /config/tokens (spec listTokens, installer).
func (s *Server) handleTokenList(w http.ResponseWriter, _ *http.Request, _ Token) {
	toks := s.tok.List()
	out := make([]tokenInfo, 0, len(toks))
	for i := range toks {
		out = append(out, tokenInfo{
			ID: toks[i].ID, Name: toks[i].Name, Role: string(toks[i].Role),
			Created: toks[i].Created, LastUsed: toks[i].LastUsed,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Items []tokenInfo `json:"items"`
	}{out})
}

// handleTokenCreate is POST /config/tokens (spec createToken,
// installer): 201 + the plaintext exactly once.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, _ Token) {
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if body.Role == "" {
		writeProblem(w, http.StatusBadRequest, badField("role", "required"))
		return
	}
	tk, plaintext, err := s.tok.Create(body.Name, Role(body.Role))
	if err != nil {
		switch {
		case errors.Is(err, errUnknownRole):
			writeProblem(w, http.StatusBadRequest, badField("role", err.Error()))
		case errors.Is(err, errTokenName):
			writeProblem(w, http.StatusBadRequest, badField("name", err.Error()))
		default:
			s.internal(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, tokenCreated{
		Token: plaintext, ID: tk.ID, Name: tk.Name,
		Role: string(tk.Role), Created: tk.Created,
	})
}

// handleTokenRevoke is DELETE /config/tokens/{id} (spec revokeToken,
// installer): 204.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request, _ Token) {
	id := r.PathValue("id")
	if err := s.tok.Revoke(id); err != nil {
		if errors.Is(err, errTokenNotFound) {
			writeNotFound(w, "no token with id "+id)
			return
		}
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
