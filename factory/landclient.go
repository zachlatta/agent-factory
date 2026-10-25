package factory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Exit codes of the land-review command.
const (
	LandOK      = 0 // approve or nits for the current HEAD: land it
	LandError   = 1 // could not get a review (or HEAD moved since): do not land
	LandChanges = 2 // blocking findings: fix, commit, run again
	LandZach    = 3 // escalate or close: do not land; tell Zach
)

// LandClient asks the factory for a landing review of the calling agent's work and waits for it.
type LandClient struct {
	Base    string                 // factory URL, e.g. http://127.0.0.1:7478
	By      string                 // the caller's PASEO_AGENT_ID
	Why     string                 // what the change is, for the reviewer
	Head    func() (string, error) // the checkout's HEAD now
	Out     io.Writer
	Poll    time.Duration
	Timeout time.Duration
	Sleep   func(time.Duration)
}

// Run returns one of the Land* exit codes.
func (c LandClient) Run() int {
	if c.By == "" {
		fmt.Fprintln(c.Out, "land-review: PASEO_AGENT_ID is not set; run this from a Paseo agent")
		return LandError
	}
	body, _ := json.Marshal(map[string]string{"by": c.By, "why": c.Why})
	req, _ := http.NewRequest("POST", c.Base+"/api/land-review", bytes.NewReader(body))
	req.Header.Set("X-Factory", "1")
	var r LandReview
	if err := c.do(req, &r); err != nil {
		fmt.Fprintln(c.Out, "land-review: request refused: "+err.Error())
		return LandError
	}
	fmt.Fprintf(c.Out, "land-review: reviewer %s is checking %s (repo %s). Waiting up to %s.\n",
		short(r.Review), short(r.Head), r.Visibility, c.Timeout)
	for waited := time.Duration(0); r.Status != "done"; waited += c.Poll {
		if waited >= c.Timeout {
			fmt.Fprintln(c.Out, "land-review: timed out waiting for the reviewer. Do not land; tell Zach.")
			return LandError
		}
		c.Sleep(c.Poll)
		get, _ := http.NewRequest("GET", c.Base+"/api/land-review/"+c.By, nil)
		if err := c.do(get, &r); err != nil {
			fmt.Fprintln(c.Out, "land-review: polling: "+err.Error())
		}
	}
	fmt.Fprintf(c.Out, "land-review: verdict %s for %s\n\n%s\n\n", r.Verdict, short(r.Head), r.Findings)
	switch r.Verdict {
	case "approve", "nits":
		head, err := c.Head()
		if err != nil || strings.TrimSpace(head) != r.Head {
			fmt.Fprintf(c.Out, "land-review: HEAD moved since the review (%s, %v). Run land-review again.\n",
				short(strings.TrimSpace(head)), err)
			return LandError
		}
		fmt.Fprintln(c.Out, "land-review: approved. Land exactly this HEAD.")
		return LandOK
	case "changes":
		fmt.Fprintln(c.Out, "land-review: fix each blocking finding, commit, and run land-review again. Do not land.")
		return LandChanges
	}
	fmt.Fprintln(c.Out, "land-review: the reviewer wants Zach to decide. Do not land; tell Zach.")
	return LandZach
}

func (c LandClient) do(req *http.Request, v any) error {
	cl := http.Client{Timeout: 2 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(raw, &e)
		return fmt.Errorf("%d %s", resp.StatusCode, e.Error)
	}
	return json.Unmarshal(raw, v)
}
