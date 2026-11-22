#!/bin/sh
# Build the latest commit of $CALIBREFAST_BRANCH (default master) while the
# server keeps running, then point `current` at it. Restarting the server
# afterwards is a second or two; the build (20-30s) happens before it.
#
#     sudo -u calibrefast deploy/update.sh && sudo systemctl restart calibrefast
#
# Layout, in the checkout this script is in (the "home", /opt/calibrefast):
#
#     releases/<commit>/   one git worktree per commit, built in place:
#                          the binary, templates and static files together
#     current -> releases/<commit>   what the systemd unit runs
#     CalibreFast -> current/CalibreFast   for running from the home itself
#
# A release reads data/ and pages/ (and cache/ and covers/, if the home has
# them) through links to the home's, so relative paths in calibrefast.conf
# and the defaults keep meaning what they did. The newest $CALIBREFAST_KEEP
# releases (default 3) are kept; going back is pointing `current` at an
# older one and restarting, and it stays there until a newer commit lands.
#
# If the build fails, nothing changes: `current` and the home stay as they
# were, and the half-built release is removed. The systemd unit also runs
# this before every start (deploy/systemd/), where it finds nothing to do
# unless something new landed since the last update.

set -eu

say() { echo "update: $*"; }

# The home is the main checkout, also when this runs from a release's copy.
here=$(cd "$(dirname "$0")" && pwd)
# (--git-common-dir may be relative to $here; git before 2.31 has no
# --path-format=absolute.)
common=$(cd "$here" && cd "$(git rev-parse --git-common-dir)" && pwd)
home=$(dirname "$common")
cd "$home"
branch="${CALIBREFAST_BRANCH:-master}"
keep="${CALIBREFAST_KEEP:-3}"

# One update at a time (a manual one and the unit's ExecStartPre).
if command -v flock >/dev/null 2>&1; then
    exec 9>"$home/.update.lock"
    if ! flock -w 600 9; then
        say "another update is still running"
        exit 1
    fi
fi

git fetch --quiet origin "$branch"
sha=$(git rev-parse "origin/$branch^{commit}")
short=$(git rev-parse --short "$sha")
rel="releases/$sha"

# Point name at target atomically (a new link renamed over the old one).
relink() {
    ln -sfn "$1" "$2.new"
    mv -Tf "$2.new" "$2" 2>/dev/null || mv -fh "$2.new" "$2"
}

# Bring the home's checkout to the commit `current` runs, for its deploy/
# files and for running from the home (templates must match the binary).
sync_home() {
    # Leftovers of a build in the home (before release folders) would stop it.
    git checkout --quiet -- static/dist
    git clean -fdq -- static/dist
    git checkout --quiet "$branch"
    git merge --ff-only --quiet "$sha" ||
        say "warning: the home checkout has local commits; left as it is"
}

if [ -x "$rel/CalibreFast" ]; then
    if [ "$(readlink current 2>/dev/null)" = "$rel" ]; then
        say "already at $short, nothing to build"
        exit 0
    fi
    if [ "$(cat releases/.last-update 2>/dev/null)" = "$sha" ]; then
        # Someone pointed `current` back at an older release: leave it.
        say "current is $(readlink current), set by hand; the latest, $short, is built"
        say "to run it: ln -sfn $rel current, then restart"
        exit 0
    fi
    say "$short was built before"
else
    say "building $short ($branch) in $rel"
    rm -rf "$rel"
    git worktree prune
    mkdir -p releases data pages
    git worktree add --quiet --detach "$rel" "$sha"
    for d in data pages cache covers; do
        if [ -d "$d" ] && [ ! -e "$rel/$d" ]; then
            ln -s "../../$d" "$rel/$d"
        fi
    done
    if (cd "$rel" && go generate . && go build -tags sqlite_fts5 -o CalibreFast.new . && mv -f CalibreFast.new CalibreFast); then
        say "built $short"
    else
        say "build failed: staying on $(readlink current 2>/dev/null || echo 'what is there')"
        git worktree remove --force "$rel" 2>/dev/null || rm -rf "$rel"
        git worktree prune
        exit 1
    fi
fi

# The first switch from the old layout: the home's binary was a plain file.
if [ -f CalibreFast ] && [ ! -L CalibreFast ]; then
    mv -f CalibreFast CalibreFast.old
fi
touch "$rel" # newest, for the pruning below
relink "$rel" current
echo "$sha" >releases/.last-update
relink current/CalibreFast CalibreFast
sync_home
say "current is $short: restart the server to run it"

# Keep the newest $keep releases, and always the one `current` points at.
ls -1t releases | tail -n +"$((keep + 1))" | while read -r old; do
    if [ "releases/$old" != "$rel" ]; then
        git worktree remove --force "releases/$old" 2>/dev/null || rm -rf "releases/$old"
        say "removed old release $(echo "$old" | cut -c1-7)"
    fi
done
git worktree prune
