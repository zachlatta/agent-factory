package factory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// All records are hand-written synthetic data, never copied session lines.
func TestTokenReaders(t *testing.T) {
	cases := []struct {
		provider, data                    string
		input, output, read, write, total int64
		events                            int
	}{
		{"claude", `{"type":"assistant","timestamp":"2026-01-01T01:00:00Z","message":{"id":"msg-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}
{"type":"assistant","timestamp":"2026-01-01T01:00:01Z","message":{"id":"msg-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}
{"type":"assistant","timestamp":"2026-01-01T01:00:00Z","message":{"id":"msg-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}
{"type":"user","message":{"usage":{"input_tokens":999}}}`, 35, 4, 20, 5, 39, 1},
		{"codex", `{"type":"turn_context","payload":{"model":"model-b"}}
{"type":"event_msg","timestamp":"2026-01-01T01:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":40,"reasoning_output_tokens":3}}}}
{"type":"token_usage_record","payload":{"usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"event_msg","timestamp":"2026-01-01T01:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"cached_input_tokens":60,"reasoning_output_tokens":7}}}}
{"type":"event_msg","timestamp":"2026-01-01T01:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"cached_input_tokens":60,"reasoning_output_tokens":7}}}}
{"type":"event_msg","timestamp":"2026-01-01T01:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":40,"reasoning_output_tokens":3}}}}`, 150, 20, 60, 0, 170, 2},
		{"muse", `{"recorded_at":1000000,"payload_type":"runtime.session","payload":{"event":{"kind":"model_completed","model":"model-c","usage":{"input_tokens":100,"output_tokens":10,"cached_tokens":40,"cache_read_tokens":40,"cache_write_tokens":5,"reasoning_tokens":3}}}}
{"recorded_at":1000000,"payload_type":"runtime.session","payload":{"event":{"kind":"model_completed","model":"model-c","usage":{"input_tokens":100,"output_tokens":10,"cached_tokens":40,"cache_read_tokens":40,"cache_write_tokens":5,"reasoning_tokens":3}}}}
{"payload_type":"runtime.session","payload":{"event":{"kind":"goal_usage_attribution","record":{"quantity":{"input_tokens":100,"output_tokens":10}}}}}`, 100, 10, 40, 5, 110, 1},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.jsonl")
			writeFile(t, path, tc.data+"\n")
			r := readTokenFile(context.Background(), path, tc.provider)
			var sum TokenCounts
			for _, e := range r.events {
				sum.add(e.Counts)
				if e.Model == "" {
					t.Error("missing event model")
				}
			}
			if r.status != "readable" || len(r.events) != tc.events || sum.Input != tc.input || sum.Output != tc.output || sum.CacheRead != tc.read || sum.CacheWrite != tc.write || sum.Total != tc.total {
				t.Fatalf("read = %+v, sum = %+v", r, sum)
			}
			if tc.provider == "codex" && (!r.events[0].From.IsZero() || r.events[1].From.IsZero()) {
				t.Fatal("cumulative intervals lost")
			}
		})
	}
}

func TestTokenReaderFailuresAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.jsonl")
	if r := readTokenFile(context.Background(), path, "claude"); r.status != "missing" {
		t.Fatal(r)
	}
	writeFile(t, path, "{broken}\n{\"type\":\"assistant\"}\n")
	if r := readTokenFile(context.Background(), path, "claude"); r.status != "error" {
		t.Fatal(r)
	}
	writeFile(t, path, `{"type":"event_msg","timestamp":"2026-01-01T01:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10}}}}
{"type":"event_msg","timestamp":"2026-01-01T01:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":2}}}}`)
	r := readTokenFile(context.Background(), path, "codex")
	if len(r.events) != 2 || r.events[1].Counts.Total != 7 || !r.events[1].From.IsZero() {
		t.Fatalf("reset = %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := readTokenFile(ctx, path, "codex"); r.status != "limited" {
		t.Fatal(r)
	}
}

func TestCodexZeroAndCacheCounterDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.jsonl")
	writeFile(t, path, `{"type":"event_msg","timestamp":"2026-01-01T01:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0}}}}`)
	if r := readTokenFile(context.Background(), path, "codex"); r.status != "readable" {
		t.Fatal(r)
	}
	writeFile(t, path, `{"type":"event_msg","timestamp":"2026-01-01T01:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":40}}}}
{"type":"event_msg","timestamp":"2026-01-01T01:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"cached_input_tokens":30}}}}`)
	r := readTokenFile(context.Background(), path, "codex")
	var total TokenCounts
	for _, e := range r.events {
		total.add(e.Counts)
	}
	if r.status != "error" || total.Total != 170 || total.CacheRead != 40 {
		t.Fatalf("counter drop = %+v %+v", r, total)
	}
}

func TestUsageStageIntervals(t *testing.T) {
	start := t0.Add(-time.Hour)
	timing := CardTiming{Partial: true, Events: []TimingEvent{{Stage: "plan", At: start}, {Stage: "building", At: start.Add(10 * time.Minute)}, {Stage: "review", At: start.Add(20 * time.Minute)}, {Stage: "building", At: start.Add(30 * time.Minute)}, {Stage: "landing", At: start.Add(40 * time.Minute)}}, Rounds: []TimingRound{{Kind: "code", N: 2, Start: start.Add(20 * time.Minute), End: start.Add(30 * time.Minute)}}}
	for _, tc := range []struct {
		from, at    int
		stage       string
		span, round int
	}{{1, 2, "plan", 1, 0}, {11, 12, "building", 2, 0}, {21, 22, "review", 3, 2}, {31, 32, "building", 4, 0}, {40, 40, "landing", 5, 0}, {9, 11, "unassigned", 0, 0}, {-2, -1, "unassigned", 0, 0}, {-100, 2, "unassigned", 0, 0}} {
		e := tokenEvent{At: start.Add(time.Duration(tc.at) * time.Minute), From: start.Add(time.Duration(tc.from) * time.Minute)}
		if tc.from == -100 {
			e.From = time.Time{}
		}
		stage, span, round := tokenStage(e, timing, t0)
		if stage != tc.stage || span != tc.span || round != tc.round {
			t.Fatalf("%+v = %s %d %d", tc, stage, span, round)
		}
	}
	if stage, _, _ := tokenStage(tokenEvent{}, timing, t0); stage != "unassigned" {
		t.Fatal(stage)
	}
}

func TestTokenFileLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(tokenFileLimit + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r := readTokenFile(context.Background(), path, "claude"); r.status != "limited" {
		t.Fatal(r.status)
	}
}
