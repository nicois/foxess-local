package api

import (
	"errors"
	"net/http"
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
	// TimeZone and UTCOffsetS: the host zone the schedule windows are
	// evaluated in (audit R8).
	TimeZone   string `json:"time_zone"`
	UTCOffsetS int    `json:"utc_offset_s"`
	// InverterClockOffsetS: the inverter's clock minus the host's at the
	// last read (positive: the inverter is ahead); null before a read.
	InverterClockOffsetS *float64 `json:"inverter_clock_offset_s"`
}

// regRTCAddr is the inverter clock block (0x9C40 ×6: year, month, day,
// hour, minute, second — profile §5), read with the settings.
const (
	regRTCAddr  = 0x9C40
	regRTCCount = 6
)

// inverterClockOffset is the inverter's clock minus the host's, from the
// newest RTC read (the inverter keeps local wall time; it is read in the
// host's zone). false: no complete read yet.
func (s *Server) inverterClockOffset() (float64, bool) {
	var v [regRTCCount]int
	var at time.Time
	for i := range regRTCCount {
		val, ts, ok, err := s.st.RegisterRead(regRTCAddr + i)
		if err != nil || !ok {
			return 0, false
		}
		v[i] = val
		if i == regRTCCount-1 {
			at = ts
		}
	}
	rtc := time.Date(v[0], time.Month(v[1]), v[2], v[3], v[4], v[5], 0, time.Local)
	return rtc.Sub(at).Round(time.Second).Seconds(), true
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
