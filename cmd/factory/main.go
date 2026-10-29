// Command factory serves the agent factory board on 127.0.0.1 and runs its pipeline.
//
//	factory [flags]                     serve the board
//	factory scan [--staged|--tree|A..B] secret-scan a git repo (see factory/scan)
//	land-review [-why ...]               (or `factory land-review`) ask the running factory for a landing
//	                                    review of the calling Paseo agent's committed work, and wait
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zachlatta/agent-factory/factory"
	"github.com/zachlatta/agent-factory/factory/scan"
)

// rev is the commit this binary was built from, set by the deployer (-ldflags "-X main.rev=<sha>").
var rev = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "retirement-inventory" {
		os.Exit(retirementCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "scan" {
		os.Exit(scanCmd(os.Args[2:]))
	}
	if filepath.Base(os.Args[0]) == "land-review" {
		os.Exit(landReview(os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "land-review" {
		os.Exit(landReview(os.Args[2:]))
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	port := flag.Int("port", 7478, "listen on 127.0.0.1:<port>")
	state := flag.String("state", filepath.Join(home, ".local/state/agent-factory"), "state directory (ideas.json)")
	boardURL := flag.String("board-url", os.Getenv("FACTORY_BOARD_URL"), "URL Zach opens the board at, for emails (env FACTORY_BOARD_URL)")
	defRepo := flag.String("default-repo", os.Getenv("FACTORY_DEFAULT_REPO"), "owner/repo for new cards with none given (env FACTORY_DEFAULT_REPO)")
	guarded := flag.String("guarded", os.Getenv("FACTORY_GUARDED_REPOS"), "comma-separated public repos to scan before landing (env FACTORY_GUARDED_REPOS; default the factory's own repo)")
	lanes := flag.String("lanes", os.Getenv("FACTORY_LANES"), "JSON file of board lane names per repo (env FACTORY_LANES; default ~/.config/agent-factory/lanes.json)")
	private := flag.String("private-rules", os.Getenv("FACTORY_PRIVATE_RULES"), "file of private scan patterns (env FACTORY_PRIVATE_RULES; default ~/.config/agent-factory/denylist)")
	zachEmail := flag.String("zach-email", os.Getenv("FACTORY_ZACH_EMAIL"), "address cards with no email thread email Zach at (env FACTORY_ZACH_EMAIL)")
	digestTo := flag.String("digest-to", os.Getenv("FACTORY_DIGEST_TO"), "address the 6 PM daily digest goes to (env FACTORY_DIGEST_TO; default -zach-email)")
	metricHosts := flag.String("metric-hosts", os.Getenv("FACTORY_METRIC_HOSTS"), "comma-separated SSH aliases whose load the board shows besides this host (env FACTORY_METRIC_HOSTS)")
	idleTTL := flag.Duration("idle-ttl", envDuration("FACTORY_IDLE_TTL"), "park a Needs Zach card idle this long (env FACTORY_IDLE_TTL; default 3h)")
	idleCap := flag.Int("idle-cap", envInt("FACTORY_IDLE_CAP"), "park the longest idle Needs Zach cards while more factory agents than this are idle (env FACTORY_IDLE_CAP; default 10)")
	previewPorts := flag.String("preview-ports", os.Getenv("FACTORY_PREVIEW_PORTS"), "lo-hi ports Ready for you previews run and are published on (env FACTORY_PREVIEW_PORTS; default 7700-7799)")
	previewMax := flag.Int("preview-max", envInt("FACTORY_PREVIEW_MAX"), "previews running at once (env FACTORY_PREVIEW_MAX; default 8)")
	tick := flag.Bool("tick", true, "run the pipeline; -tick=false only serves the board (deploy canary)")
	listen := flag.String("listen", "", "address to serve on (default 127.0.0.1:<port>; host:0 picks a free port)")
	stageHandoff := flag.String("stage-handoff", envOr("FACTORY_STAGE_HANDOFF", "on"), "on: each new stage of a card starts a fresh agent with a short handoff; off: resume the card's session (env FACTORY_STAGE_HANDOFF)")
	dryRun := flag.Bool("dry-run", false, "serve real data, change nothing: buttons only log the paseo and git writes they would make")
	flag.Parse()

	cfg := factory.Config{Host: host, Home: home, StateDir: *state, Port: *port, BoardURL: *boardURL,
		DefaultRepo: *defRepo, PrivateRules: *private, LanesFile: *lanes, Rev: rev, ReadOnly: !*tick, ZachEmail: *zachEmail, DigestTo: *digestTo,
		DryRun: *dryRun, IdleTTL: *idleTTL, IdleCap: *idleCap, PreviewPorts: *previewPorts, PreviewMax: *previewMax,
		NoStageHandoff: *stageHandoff == "off"}
	if *metricHosts != "" {
		cfg.MetricHosts = splitList(*metricHosts)
	}
	if *guarded != "" {
		cfg.Guarded = splitList(*guarded)
	}
	factory.InboxSource, factory.InboxAnchorRole = os.Getenv("FACTORY_INBOX_SOURCE"), os.Getenv("FACTORY_INBOX_ANCHOR_ROLE")
	if *dryRun {
		dir, err := factory.DryRunState(*state)
		if err != nil {
			log.Fatal(err)
		}
		cfg.StateDir = dir
		log.Printf("dry run: nothing changes; state copied to %s", dir)
	}
	f := factory.New(cfg, factory.CLI{ArchiveBin: os.Getenv("FACTORY_ARCHIVE_CLI")})
	if !*dryRun {
		f.Mail = factory.PDWMailer{}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", listenAddr(*listen, *port))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("agent factory %s on http://%s", rev, ln.Addr())
	err = f.Serve(ctx, ln, f.Handler(), 60*time.Second)
	if *dryRun {
		if rmErr := factory.RemoveDryRunState(cfg.StateDir); rmErr != nil {
			log.Print(rmErr)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	log.Print("agent factory stopped")
}

func landReview(args []string) int {
	fs := flag.NewFlagSet("land-review", flag.ExitOnError)
	why := fs.String("why", "", "what the change is and why, for the reviewer")
	url := fs.String("url", "http://127.0.0.1:7478", "factory URL")
	wait := fs.Duration("timeout", 30*time.Minute, "how long to wait for the verdict")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: land-review [-why text]\n\nRun from a Paseo agent after committing (worktree clean), "+
			"before landing on main.\nExit 0: approved, land exactly this HEAD. 2: changes requested. 3: Zach decides. 1: error.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	return factory.LandClient{
		Base: *url, By: os.Getenv("PASEO_AGENT_ID"), Why: *why, Out: os.Stdout,
		Poll: 20 * time.Second, Timeout: *wait, Sleep: time.Sleep,
		Head: func() (string, error) {
			out, err := exec.Command("git", "rev-parse", "HEAD").Output()
			return string(out), err
		},
	}.Run()
}

// listenAddr is the address to serve on: listen if given, else 127.0.0.1:<port>.
func listenAddr(listen string, port int) string {
	if listen != "" {
		return listen
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// scanCmd: exit 0 clean, 1 hits, 2 the scan could not run.
func scanCmd(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	dir := fs.String("C", ".", "repo to scan")
	staged := fs.Bool("staged", false, "scan the index and the next commit's identity (pre-commit)")
	tree := fs.Bool("tree", false, "scan every tracked file and every commit's identity")
	cfgPath := fs.String("config", "", "gitleaks config file (default: gitleaks' own rules)")
	home, _ := os.UserHomeDir()
	private := fs.String("private-rules", envOr("FACTORY_PRIVATE_RULES", filepath.Join(home, ".config/agent-factory/denylist")),
		"file of private patterns")
	requirePrivate := fs.Bool("require-private", false, "fail if the private rules file is missing")
	fs.Parse(args)
	o := scan.Options{Staged: *staged, Tree: *tree, GitleaksConfig: *cfgPath}
	if fs.NArg() == 1 {
		o.Range = fs.Arg(0)
	}
	priv, err := scan.LoadPrivate(*private)
	switch {
	case err == nil:
		o.Private = priv
	case *requirePrivate:
		fmt.Fprintf(os.Stderr, "scan: private rules: %v\n", err)
		return 2
	default:
		fmt.Fprintln(os.Stderr, "scan: private rules not loaded; generic rules only")
	}
	hits, err := scan.Run(context.Background(), *dir, o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
		return 2
	}
	for _, h := range hits {
		fmt.Println(h)
	}
	if len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "scan: %d finding(s); nothing here may be published\n", len(hits))
		return 1
	}
	return 0
}

// envDuration and envInt read an optional setting; a bad value stops the factory at start.
func envDuration(k string) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return d
}

func envInt(k string) int {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return n
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
