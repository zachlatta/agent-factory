package factory

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
)

// Lane is how the board shows one repo: a short name, how work lands there, and how many of its
// ideas the factory starts on its own.
type Lane struct {
	Name  string    `json:"name"`
	Lands string    `json:"lands,omitempty"`
	Ideas *IdeaCaps `json:"ideas,omitempty"`
}

// IdeaCaps limits the ideas the factory starts on its own in one repo. A missing field takes its
// default; 0 means never.
type IdeaCaps struct {
	InFlight *int `json:"inFlight,omitempty"` // its idea cards in flight at once (default 2)
	Daily    *int `json:"daily,omitempty"`    // ideas it starts per day (default 5)
}

// RepoIdeaCaps is a repo's caps with the defaults filled in.
type RepoIdeaCaps struct {
	InFlight int `json:"inFlight"`
	Daily    int `json:"daily"`
}

const (
	defaultIdeaInFlight = 2
	defaultIdeaDaily    = 5
)

func (l Lane) ideaCaps() RepoIdeaCaps {
	c := RepoIdeaCaps{InFlight: defaultIdeaInFlight, Daily: defaultIdeaDaily}
	if l.Ideas != nil {
		if l.Ideas.InFlight != nil {
			c.InFlight = max(0, *l.Ideas.InFlight)
		}
		if l.Ideas.Daily != nil {
			c.Daily = max(0, *l.Ideas.Daily)
		}
	}
	return c
}

// lanes reads Config.LanesFile: {"owner/repo": {"name": "...", "lands": "...", "ideas": {...}}}. Repo names and
// how they deploy are host config, not code: some of them are private repos.
func (f *Factory) lanes() map[string]Lane {
	out := map[string]Lane{}
	if b, err := os.ReadFile(f.cfg.LanesFile); err == nil {
		if err := json.Unmarshal(b, &out); err != nil {
			f.logf("lanes file %s: %v", f.cfg.LanesFile, err)
		}
	}
	return out
}

func (f *Factory) handleLanes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f.lanes())
}

// errandsName is the short name of the repo where errands are recorded (Config.DefaultRepo).
func (f *Factory) errandsName() string {
	if f.cfg.DefaultRepo == "" {
		return "the default repo's"
	}
	return path.Base(f.cfg.DefaultRepo)
}
