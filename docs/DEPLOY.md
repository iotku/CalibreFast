# Running CalibreFast as a service

This guide covers running CalibreFast on a Linux server with systemd. The service:

- pulls the latest commit of `master`, rebuilds the browser bundles (`go generate`) and builds the server with full-text search (`-tags sqlite_fts5`) every time it starts;
- reads its options from a config file, `calibrefast.conf`;
- starts again whenever it stops on its own, including when an admin stops it from the web (**Account menu → Server → Stop server**).

So restarting the service, or pressing **Stop server**, also updates it.

## The config file

Any CalibreFast can read its options from a file instead of the command line:

```bash
cp calibrefast.conf.example calibrefast.conf
./CalibreFast -config calibrefast.conf
```

Each line is `name = value`, with the same names as the [command-line flags](../README.md#getting-started). An on/off flag can be just its name (`allow-writes`). Lines starting with `#` are comments. An option given on the command line as well overrides the file's value. `calibrefast.conf` is in `.gitignore`, so your settings stay out of git and a `git pull` never touches them.

The flags that run a one-off task and exit (`-create-user`, `-admin`, `-empty-recycle-bin`) only work on the command line. They do read the rest of the options from the file, so `-datadir` and `-basedir` match the server's:

```bash
./CalibreFast -config calibrefast.conf -create-user yourname -admin
```

Secrets (OAuth client secrets, `CF_GOOGLE_BOOKS_KEY`) are environment variables, not options, so they don't show up in process lists. Copy `calibrefast.env.example` to `calibrefast.env` (also ignored by git) and fill in what you use. The systemd unit reads it.

## Setting up the systemd service

The steps below install into `/opt/calibrefast`, running as a `calibrefast` user. Change the paths in the unit file if yours differ.

1. **Install Go** (1.26 or newer, see `go.mod`), for example to `/usr/local/go`. The service builds CalibreFast itself, so Go stays installed.

2. **Create the user and the checkout.** The checkout is used only to run CalibreFast: the update script resets the built files in `static/dist/` before pulling.

   ```bash
   sudo useradd --system --create-home --home-dir /var/lib/calibrefast calibrefast
   sudo git clone https://github.com/iotku/CalibreFast.git /opt/calibrefast
   sudo chown -R calibrefast:calibrefast /opt/calibrefast
   ```

   The `calibrefast` user needs to read (and, with `allow-writes`, write) the Calibre library, and to write `datadir` and `cachedir`.

3. **Write the config** and, if you use them, the secrets:

   ```bash
   cd /opt/calibrefast
   sudo -u calibrefast cp calibrefast.conf.example calibrefast.conf
   sudo -u calibrefast "$EDITOR" calibrefast.conf
   # optional
   sudo -u calibrefast cp calibrefast.env.example calibrefast.env
   sudo -u calibrefast chmod 600 calibrefast.env
   ```

4. **Build once and create the first admin:**

   ```bash
   sudo -u calibrefast PATH=/usr/local/go/bin:$PATH ./deploy/update.sh
   sudo -u calibrefast ./CalibreFast -config calibrefast.conf -create-user yourname -admin
   ```

5. **Install and start the service:**

   ```bash
   sudo cp deploy/systemd/calibrefast.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now calibrefast
   ```

   Watch it with `systemctl status calibrefast` and `journalctl -u calibrefast -f`.

### What happens on each start

`ExecStartPre` runs [`deploy/update.sh`](../deploy/update.sh):

1. It resets `static/dist/` to the committed files, so leftovers from an earlier `go generate` can't block the pull.
2. It fetches and fast-forwards to `origin/$CALIBREFAST_BRANCH` (`master` unless the unit sets otherwise).
3. If that brought a new commit (or there's no binary yet), it runs `go generate .` (type-check plus bundles) and `go build -tags sqlite_fts5`. The new binary replaces the old one only once it has built.
4. If the build fails, the checkout goes back to the commit it was on. Templates and static files are read from the checkout while the server runs, so they have to match the binary.

The unit runs the script with a leading `-`, so a failed update (no network, a build error) never keeps CalibreFast down. The build that's already there starts, and the failure is in the journal.

The first start downloads Go modules and the TypeScript tools, which can take a few minutes (`TimeoutStartSec=15min`).

### Stopping, restarting, updating

| You want to… | Do |
|---|---|
| Update to the latest `master` | `sudo systemctl restart calibrefast`, or **Stop server** on the Server page |
| Stop it (it stays stopped) | `sudo systemctl stop calibrefast` |
| Change an option | Edit `calibrefast.conf`, then restart |
| Run another branch | `Environment=CALIBREFAST_BRANCH=…` in the unit (`systemctl edit calibrefast`), then restart |

Either way of stopping is clean: CalibreFast stops taking new connections, gives requests in progress (a download, an upload) up to 30 seconds to finish, closes its databases and exits. On `systemctl stop`, systemd sends `SIGTERM` and waits up to `TimeoutStopSec`, which is 45 seconds.

## Stopping from the web

Admins have **Server** in the account menu (`/admin/server`). The page shows the version that's running and since when, and has a **Stop server** button. Only admins can open the page or call it (`POST /api/admin/server/stop`): editors, users and visitors get a 404, as for any admin page.

The page says whether systemd started the server:

- **Started by systemd:** after **Stop server**, the page waits for the server to come back up (pulled and rebuilt) and reloads.
- **Anything else:** stopping really stops it. Nothing starts it again until someone does so on the server, and the page warns about this before stopping.

## Without systemd

The update script and the config file work anywhere:

```bash
./deploy/update.sh && ./CalibreFast -config calibrefast.conf
```

Any supervisor that restarts a process when it exits (launchd, runit, s6, supervisord, a container's restart policy) can take systemd's place. Have it run the line above, so that it restarts on exit status 0 too.

**Planned:** a way for CalibreFast to restart itself after **Stop server** when no service manager runs it. That would let the button update a server started by hand. It isn't implemented yet: without a supervisor, **Stop server** only stops.
