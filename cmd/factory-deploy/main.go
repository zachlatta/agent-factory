// Command factory-deploy keeps the agent factory running what is on its repo's main (package deployer).
// The agent-factory-deploy timer runs it every minute; ./install.sh runs it once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/zachlatta/agent-factory/deployer"
)

func main() {
	home, _ := os.UserHomeDir()
	share := filepath.Join(home, ".local/share/agent-factory")
	remote := flag.String("remote", "https://github.com/zachlatta/agent-factory.git", "git URL of the factory's repo")
	branch := flag.String("branch", "main", "branch to deploy")
	subdir := flag.String("subdir", "", "factory module inside the repo (empty: the repo root)")
	src := flag.String("src", filepath.Join(share, "src"), "the deployer's own sparse clone")
	bin := flag.String("bin", share, "directory holding factory, factory-deploy and good/")
	unitDir := flag.String("unit-dir", filepath.Join(home, ".config/systemd/user"), "systemd user unit directory")
	units := flag.String("units", "agent-factory.service,agent-factory-deploy.service,agent-factory-deploy.timer", "unit files in <subdir>/deploy to install")
	service := flag.String("service", "agent-factory", "unit restarted on deploy")
	state := flag.String("state", filepath.Join(home, ".local/state/agent-factory/deploy.json"), "deploy state (the board shows it)")
	health := flag.String("health", "http://127.0.0.1:7478/api/health", "the factory's health URL")
	serve := flag.String("serve", "tailscale serve --bg --https=7443 http://127.0.0.1:7478", "command that publishes the board after a deploy (empty: none)")
	retry := flag.Bool("retry", false, "try a failed build again now, whatever its backoff")
	testAlert := flag.Bool("test-alert", false, "send one push labelled as a test, then exit")
	flag.Parse()

	if os.Getenv("XDG_RUNTIME_DIR") == "" { // systemctl --user needs it outside a login session
		os.Setenv("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid()))
	}
	d := deployer.New(deployer.Config{Remote: *remote, Branch: *branch, Src: *src, Subdir: *subdir, Bin: *bin,
		UnitDir: *unitDir, Units: split(*units, ","), Service: *service, StateFile: *state, HealthURL: *health,
		Serve: strings.Fields(*serve), Retry: *retry})
	if *testAlert {
		if err := d.TestAlert(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := d.Once(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func split(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
