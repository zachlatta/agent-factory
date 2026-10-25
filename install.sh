#!/bin/sh
# First install of the agent factory: check the host config, build the deployer, install and start
# its timer, then run one deploy. The deployer builds this repo's main from GitHub (never this
# checkout), so landed work goes live within a minute of landing, and this script is not needed
# after the first time. Host settings live in ~/.config/agent-factory/ (README.md).
# Logs: journalctl --user -u agent-factory -u agent-factory-deploy
set -eu
cd "$(dirname "$0")"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
conf="$HOME/.config/agent-factory"
if [ ! -s "$conf/env" ] || [ ! -s "$conf/denylist" ]; then
	echo "install: need $conf/env and $conf/denylist (README.md, Host config)" >&2
	exit 1
fi
command -v gitleaks >/dev/null || { echo "install: gitleaks not on PATH (README.md)" >&2; exit 1; }
# the hooks scan every commit and push in this public repo
git config core.hooksPath .githooks
go vet ./...
go test ./...
dest="$HOME/.local/share/agent-factory"
units="$HOME/.config/systemd/user"
mkdir -p "$dest" "$units"
go build -o "$dest/factory-deploy.new" ./cmd/factory-deploy
mv "$dest/factory-deploy.new" "$dest/factory-deploy"
cp deploy/agent-factory-deploy.service deploy/agent-factory-deploy.timer "$units/"
systemctl --user daemon-reload
"$dest/factory-deploy" -retry
systemctl --user enable agent-factory >/dev/null 2>&1
mkdir -p "$HOME/.local/bin"
ln -sf "$dest/factory" "$HOME/.local/bin/land-review" # the deployer swaps factory in place, so the link stays good
systemctl --user enable --now agent-factory-deploy.timer
curl -fsS http://127.0.0.1:7478/api/health || { echo "agent factory not healthy; see journalctl --user -u agent-factory-deploy" >&2; exit 1; }
echo
echo "agent factory: $(sed -n 's/^FACTORY_BOARD_URL=//p' "$conf/env")"
