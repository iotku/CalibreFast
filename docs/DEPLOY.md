# Running CalibreFast as a service

This guide covers running CalibreFast on a Linux server with systemd. The service:

- runs the build that `current` points at: [`deploy/update.sh`](../deploy/update.sh) builds each new commit of `master` in its own folder while the server keeps running, so an update costs a restart of a second or two, not the 20–30 seconds of the build;
- with the optional socket unit, keeps the port open while it restarts, so visitors wait a moment instead of getting an error;
- reads its options from a config file, `calibrefast.conf`;
- starts again whenever it stops on its own, including when an admin stops it from the web (**Account menu → Server → Stop server**).

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

5. **Install and start the service** and its socket. The socket's `ListenStream` (8080 in the file) is the port from now on; edit it first if yours differs. Without the socket the service works as well, listening on `port` from `calibrefast.conf` itself.

   ```bash
   sudo cp deploy/systemd/calibrefast.service deploy/systemd/calibrefast.socket /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now calibrefast.socket calibrefast
   ```

   Watch it with `systemctl status calibrefast` and `journalctl -u calibrefast -f`. The first line it logs says where it listens: `Listening on [::]:8080 (socket from systemd; -port is not used)` with the socket.

### How updates work

The server reads its templates and static files from disk while it runs, so the binary and those files have to come from the same commit. [`deploy/update.sh`](../deploy/update.sh) keeps each build in a folder of its own:

```
/opt/calibrefast/                 the git checkout (the "home"): config, data, the script
├── releases/<commit>/            one git worktree per commit, with its binary
├── current -> releases/<commit>  the build the service runs
└── CalibreFast -> current/CalibreFast
```

1. It fetches `origin/$CALIBREFAST_BRANCH` (`master` unless the unit sets otherwise). If `current` is already that commit, it stops there.
2. It checks the commit out in `releases/<commit>/` and runs `go generate .` (type-check plus bundles) and `go build -tags sqlite_fts5` there. The running server doesn't notice: its files are in another folder.
3. If the build fails, the half-built folder is removed and nothing else changes.
4. If it built, `current` switches to it (atomically), and the home checkout fast-forwards to the same commit. The newest three releases are kept (`CALIBREFAST_KEEP`).

The new build runs once the service restarts. Each release reaches the home's `data/` and `pages/` (and `cache/`, `covers/` if there) through links, so relative paths in `calibrefast.conf`, and the default `datadir = data`, mean what they did; absolute paths are clearer.

The unit also runs the script as `ExecStartPre`, with a leading `-` so a failed update (no network, a build error) never keeps CalibreFast down. After an update it finds nothing to do. After **Stop server**, it still pulls and builds then, while the server is down (the slow way, as before).

The first start downloads Go modules and the TypeScript tools, which can take a few minutes (`TimeoutStartSec=15min`). Later builds reuse Go's caches and take seconds to half a minute.

### Stopping, restarting, updating

| You want to… | Do |
|---|---|
| Update to the latest `master` | `sudo -u calibrefast /opt/calibrefast/deploy/update.sh && sudo systemctl restart calibrefast` (builds first: down for a second or two) |
| Update, the slow way | `sudo systemctl restart calibrefast`, or **Stop server** on the Server page (builds while it's down) |
| Go back to the previous build | `sudo -u calibrefast ln -sfn releases/<commit> /opt/calibrefast/current`, then restart (`ls -t releases` lists them, newest first). It stays on that build until a newer commit lands |
| Stop it (it stays stopped) | `sudo systemctl stop calibrefast calibrefast.socket` (the socket alone would start it again on the next visit) |
| Change an option | Edit `calibrefast.conf`, then restart |
| Change the port | Edit `ListenStream` in `calibrefast.socket` (`systemctl edit --full calibrefast.socket`), then `systemctl restart calibrefast.socket calibrefast` |
| Run another branch | `Environment=CALIBREFAST_BRANCH=…` in the unit (`systemctl edit calibrefast`), then update |

Either way of stopping is clean: CalibreFast stops taking new connections, gives requests in progress (a download, an upload) up to 30 seconds to finish, closes its databases and exits. On `systemctl stop`, systemd sends `SIGTERM` and waits up to `TimeoutStopSec`, which is 45 seconds.

With `calibrefast.socket`, "stops taking new connections" only means the old process stops accepting them: the port stays open, connections wait in its queue, and the new process answers them once it's up. A restart then shows as a short pause, not an error page. Without the socket, connections are refused from the moment the old process starts stopping until the new one listens.

### Moving from the old layout

Before releases, the service built in the checkout itself and ran `/opt/calibrefast/CalibreFast`. To move:

```bash
cd /opt/calibrefast
sudo -u calibrefast git pull --ff-only
sudo -u calibrefast PATH=/usr/local/go/bin:$PATH ./deploy/update.sh
sudo cp deploy/systemd/calibrefast.service deploy/systemd/calibrefast.socket /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now calibrefast.socket
sudo systemctl restart calibrefast
```

Merge any changes you made to the old unit (paths, user, `CALIBREFAST_BRANCH`) into the new one, and set the socket's port to yours. The old binary is kept as `CalibreFast.old`; delete it once the new one runs. Until the unit is replaced, the old one keeps working: `CalibreFast` in the checkout is now a link to `current`'s binary, and the checkout is kept at the same commit.

## Stopping from the web

Admins have **Server** in the account menu (`/admin/server`). The page shows the version that's running and since when, and has a **Stop server** button. Only admins can open the page or call it (`POST /api/admin/server/stop`): editors, users and visitors get a 404, as for any admin page.

The page says whether systemd started the server:

- **Started by systemd:** after **Stop server**, the page waits for the server to come back up (pulled and rebuilt) and reloads.
- **Anything else:** stopping really stops it. Nothing starts it again until someone does so on the server, and the page warns about this before stopping.

## Backups and restoring

The server backs itself up (`backup.go`), into `-backup-dir` (default `backups` in `-datadir`; another disk is better):

- **The app database** (`calibrefast.db`: accounts, reading, bookmarks, highlights, shelves, which exist nowhere else), daily, as `app-YYYYMMDD-HHMMSS.db`.
- **Calibre's `metadata.db`**, weekly, as `library-YYYYMMDD-HHMMSS.db`, and only if it changed since the last one. Calibre's own backups still matter: this is the database, not the books.

The newest 7 of each are kept (`-backup-keep`); `-backups=false` turns them off. Times are UTC. Each is a whole SQLite database made while the server runs (`VACUUM INTO`). An admin can make one now, or download one, under **Backups** on the Server page.

`secrets.key` (the key users' Hardcover sign-ins are encrypted with) is never put in `-backup-dir` or a download: with the database it opens every linked sign-in ([SECURITY.md](SECURITY.md#where-data-is-stored)). It never changes, so only one copy is kept per key, in `secrets.key.d/` beside it in `-datadir`. Each app backup's `.json` says which key it was made with (`"key"`, the start of its SHA-256); without it, users only link Hardcover again.

**Restoring the app database:**

1. Stop the server.
2. In `-datadir`, delete `calibrefast.db-wal` and `calibrefast.db-shm` if they're there, and replace `calibrefast.db` with the backup (renamed to `calibrefast.db`, mode `0600`).
3. If `secrets.key` is gone or differs from the backup's key, copy `secrets.key.d/secrets-<key>.key` to `secrets.key`.
4. Start the server.

**Restoring `metadata.db`:** stop the server and Calibre, keep the current `metadata.db` aside, copy the backup into the library as `metadata.db` (with no `metadata.db-wal` beside it), and start them. Books added after the backup are still in their folders; Calibre's **Library maintenance → Restore database** can find them.

## Without systemd

The update script and the config file work anywhere:

```bash
./deploy/update.sh && ./CalibreFast -config calibrefast.conf
```

(`./CalibreFast` is a link to `current/CalibreFast`, and the checkout is kept at the commit it was built from.) Any supervisor that restarts a process when it exits (launchd, runit, s6, supervisord, a container's restart policy) can take systemd's place. Have it run the line above, so that it restarts on exit status 0 too.

**Planned:** a way for CalibreFast to restart itself after **Stop server** when no service manager runs it. That would let the button update a server started by hand. It isn't implemented yet: without a supervisor, **Stop server** only stops.
