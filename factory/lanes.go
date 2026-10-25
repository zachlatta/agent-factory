package factory

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
)

// Lane is how the board shows one repo: a short name, and how work lands there.
type Lane struct {
	Name  string `json:"name"`
	Lands string `json:"lands,omitempty"`
}

// lanes reads Config.LanesFile: {"owner/repo": {"name": "...", "lands": "..."}}. Repo names and
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
