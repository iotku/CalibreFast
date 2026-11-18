#!/bin/sh
# Bring a CalibreFast checkout up to date and rebuild it: pull the latest
# commit of $CALIBREFAST_BRANCH (default master), rebuild the browser
# bundles (go generate), and build the server with full-text search
# (-tags sqlite_fts5). The systemd unit (deploy/systemd/) runs it before
# every start; it works on its own too.
#
# Meant for a checkout used only to run CalibreFast: it resets static/dist/
# (built files, rebuilt here anyway). Templates and static files are read
# from the checkout at run time, so if the build fails the checkout goes
# back to the commit it was on, to match the binary already there.

set -eu

cd "$(dirname "$0")/.."
branch="${CALIBREFAST_BRANCH:-master}"

say() { echo "update: $*"; }

# Leftovers of an earlier go generate would stop the pull.
git checkout --quiet -- static/dist
git clean -fdq -- static/dist

prev=$(git rev-parse HEAD)
git fetch --quiet origin "$branch"
git checkout --quiet "$branch"
git merge --ff-only --quiet "origin/$branch"
now=$(git rev-parse HEAD)

if [ "$prev" = "$now" ] && [ -x ./CalibreFast ]; then
    say "already at $(git rev-parse --short HEAD), nothing to build"
    exit 0
fi

say "building $(git rev-parse --short HEAD) ($branch)"
if go generate . && go build -tags sqlite_fts5 -o CalibreFast.new . && mv -f CalibreFast.new CalibreFast; then
    say "built $(git rev-parse --short HEAD)"
else
    say "build failed: going back to $(git rev-parse --short "$prev")"
    rm -f CalibreFast.new
    git reset --hard --quiet "$prev"
    git checkout --quiet -- static/dist
    git clean -fdq -- static/dist
    exit 1
fi
