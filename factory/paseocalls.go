package factory

import (
	"context"
	"sort"
	"sync"
	"time"
)

// callWindow: /api/health reports the paseo calls made in this window (paseoCalls).
const callWindow = 10 * time.Minute

// PaseoCalls is the paseo calls the factory made in the last callWindow: how many, how much wall
// time, and the same per verb ("permit ls", "inspect", "agent update", ...). CLICalls ran the paseo
// CLI (a process each); HelperCalls were served by the Paseo helper (paseohelper.go), whose state
// is Helper. A call a dry run skipped is in neither.
type PaseoCalls struct {
	Window      string              `json:"window"`
	Calls       int                 `json:"calls"`
	CLICalls    int                 `json:"cliCalls"`
	HelperCalls int                 `json:"helperCalls"`
	PerMinute   float64             `json:"perMinute"`
	Seconds     float64             `json:"seconds"`
	ByVerb      map[string]VerbCall `json:"byVerb,omitempty"`
	Helper      *HelperStatus       `json:"helper,omitempty"`
}

type VerbCall struct {
	Calls   int     `json:"calls"`
	CLI     int     `json:"cli"`
	Helper  int     `json:"helper"`
	Seconds float64 `json:"seconds"`
}

type callRecord struct {
	at   time.Time
	verb string
	dur  time.Duration
	path string // callCLI, callHelper, or "" (not run, as a dry run's skipped write)
}

// Which path served a call: rawPaseo passes a *string in the call's context, and the CLI or the
// helper sets it (markCall).
const (
	callCLI    = "cli"
	callHelper = "helper"
)

type callPathKey struct{}

func withCallPath(ctx context.Context, path *string) context.Context {
	return context.WithValue(ctx, callPathKey{}, path)
}

func markCall(ctx context.Context, path string) {
	if p, ok := ctx.Value(callPathKey{}).(*string); ok {
		*p = path
	}
}

type callLog struct {
	mu   sync.Mutex
	recs []callRecord
}

// callVerb names a call by its command: two words for the CLI's command groups, one otherwise.
func callVerb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "agent", "permit", "provider", "schedule", "workspace", "terminal":
		if len(args) > 1 {
			return args[0] + " " + args[1]
		}
	}
	return args[0]
}

func (l *callLog) add(at time.Time, args []string, dur time.Duration, path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, callRecord{at, callVerb(args), dur, path})
	l.prune(at)
}

func (l *callLog) prune(now time.Time) {
	i := sort.Search(len(l.recs), func(i int) bool { return now.Sub(l.recs[i].at) <= callWindow })
	if i > 0 {
		l.recs = append(l.recs[:0], l.recs[i:]...)
	}
}

func (l *callLog) report(now time.Time) PaseoCalls {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	r := PaseoCalls{Window: callWindow.String(), ByVerb: map[string]VerbCall{}}
	for _, c := range l.recs {
		v := r.ByVerb[c.verb]
		v.Calls++
		v.Seconds += c.dur.Seconds()
		switch c.path {
		case callCLI:
			v.CLI++
			r.CLICalls++
		case callHelper:
			v.Helper++
			r.HelperCalls++
		}
		r.ByVerb[c.verb] = v
		r.Calls++
		r.Seconds += c.dur.Seconds()
	}
	r.PerMinute = float64(r.Calls) / callWindow.Minutes()
	return r
}
