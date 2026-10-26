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
	"strings"
	"syscall"
	"time"

	"github.com/zachlatta/agent-factory/factory"
	"github.com/zachlatta/agent-factory/factory/scan"
)

// rev is the commit this binary was built from, set by the deployer (-ldflags "-X main.rev=<sha>").
var rev = "dev"

func main() {
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
	tick := flag.Bool("tick", true, "run the pipeline; -tick=false only serves the board (deploy canary)")
	flag.Parse()

	cfg := factory.Config{Host: host, Home: home, StateDir: *state, Port: *port, BoardURL: *boardURL,
		DefaultRepo: *defRepo, PrivateRules: *private, LanesFile: *lanes, Rev: rev, ReadOnly: !*tick, ZachEmail: *zachEmail}
	if *guarded != "" {
		cfg.Guarded = splitList(*guarded)
	}
	factory.InboxSource, factory.InboxAnchorRole = os.Getenv("FACTORY_INBOX_SOURCE"), os.Getenv("FACTORY_INBOX_ANCHOR_ROLE")
	f := factory.New(cfg, factory.CLI{})
	f.Mail = factory.PDWMailer{}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("agent factory %s on http://%s", rev, ln.Addr())
	if err := f.Serve(ctx, ln, f.Handler(), 60*time.Second); err != nil {
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
