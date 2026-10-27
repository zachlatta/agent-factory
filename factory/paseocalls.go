package factory

import (
	"sort"
	"sync"
	"time"
)

// callWindow: /api/health reports the paseo calls made in this window (paseoCalls).
const callWindow = 10 * time.Minute

// PaseoCalls is the paseo CLI calls the factory made in the last callWindow: how many, how much
// wall time, and the same per verb ("permit ls", "inspect", "agent update", ...).
type PaseoCalls struct {
	Window    string              `json:"window"`
	Calls     int                 `json:"calls"`
	PerMinute float64             `json:"perMinute"`
	Seconds   float64             `json:"seconds"`
	ByVerb    map[string]VerbCall `json:"byVerb,omitempty"`
}

type VerbCall struct {
	Calls   int     `json:"calls"`
	Seconds float64 `json:"seconds"`
}

type callRecord struct {
	at   time.Time
	verb string
	dur  time.Duration
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

func (l *callLog) add(at time.Time, args []string, dur time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, callRecord{at, callVerb(args), dur})
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
		r.ByVerb[c.verb] = v
		r.Calls++
		r.Seconds += c.dur.Seconds()
	}
	r.PerMinute = float64(r.Calls) / callWindow.Minutes()
	return r
}
