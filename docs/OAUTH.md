# Setting up GitHub and Google sign-in

CalibreFast can let people sign in with GitHub and/or Google as well as a password. You don't need an API token. You register CalibreFast with each provider as an **OAuth app**, which gives you a **client ID** and a **client secret** to pass to CalibreFast.

Each provider is optional and independent. Set up one, both, or neither.

## Before you start: your public URL

GitHub and Google send people back to CalibreFast after they sign in, so CalibreFast needs to know the exact address people use to reach it. Pass it with `-public-url`:

| How you reach CalibreFast | `-public-url` |
|---|---|
| Your own machine, for testing | `http://localhost:8080` |
| A Tailscale network (with `tailscale serve` for HTTPS) | `https://books.your-tailnet.ts.net` |
| Behind a reverse proxy with a domain | `https://books.example.com` |

- **It must match what's in the browser's address bar exactly:** `http` vs `https`, the host name, and any port. A mismatch is the most common cause of sign-in failing.
- **Just the scheme, host and optional port,** like `https://books.example.com`. A trailing `/` is fine, but don't add a path.
- **Use HTTPS for anything but `localhost`.** Google requires it, and CalibreFast marks its sign-in cookies `Secure` when `-public-url` starts with `https://`.

The callback URLs you register with the providers are then:

- GitHub: `<public-url>/auth/github/callback`
- Google: `<public-url>/auth/google/callback`

## GitHub

1. Go to **GitHub → Settings → Developer settings → OAuth Apps → New OAuth App**, or open <https://github.com/settings/applications/new> directly. To own the app as an organisation instead, use the organisation's **Settings → Developer settings**.
2. Fill in:

   | Field | Value |
   |---|---|
   | Application name | Anything, e.g. `CalibreFast` |
   | Homepage URL | your `-public-url` |
   | Authorization callback URL | `<public-url>/auth/github/callback` |
   | Enable Device Flow | leave unticked |

3. Click **Register application**.
4. Copy the **Client ID**.
5. Click **Generate a new client secret** and copy it straight away. GitHub only shows it once; if you lose it, generate a new one.

Make sure you create an *OAuth App*, not a *GitHub App*.

CalibreFast asks GitHub for read-only access to the person's profile (`read:user`) and email address (`user:email`). People see this on GitHub's approval screen the first time.

## Google

1. Open the [Google Cloud Console](https://console.cloud.google.com/) and create a project (or pick an existing one).
2. Set up the **OAuth consent screen** (**APIs & Services → OAuth consent screen**, also called **Google Auth Platform**):
   - **User type:** *External* (or *Internal* if everyone has an account in your Google Workspace).
   - Fill in the app name and the support and contact emails.
   - **Scopes:** add `openid`, `.../auth/userinfo.email` and `.../auth/userinfo.profile`. These are basic, so Google doesn't need to verify the app.
   - **Publishing status:** while it's *Testing*, only the Google accounts you list under **Test users** can sign in. That's a good fit for a small trusted group. Click **Publish app** to allow any Google account. For these basic scopes publishing doesn't need Google's review, but people may see an "unverified app" notice.
3. Go to **APIs & Services → Credentials → Create credentials → OAuth client ID**:
   - **Application type:** *Web application*.
   - **Authorized redirect URIs:** `<public-url>/auth/google/callback`.
   - Leave **Authorized JavaScript origins** empty.
4. Click **Create** and copy the **Client ID** and **Client secret**.

Google only accepts `https://` redirect URIs, except for `http://localhost`. It also requires a public domain, so a `*.ts.net` Tailscale name or your own domain works, but a bare IP address or a `.local` name doesn't.

## Start CalibreFast with the credentials

Pass the client IDs and secrets as **environment variables**, not command-line flags, so the secrets don't appear in process lists:

```bash
export CF_GITHUB_CLIENT_ID=your-github-client-id
export CF_GITHUB_CLIENT_SECRET=your-github-client-secret
export CF_GOOGLE_CLIENT_ID=your-id.apps.googleusercontent.com
export CF_GOOGLE_CLIENT_SECRET=your-google-client-secret

./CalibreFast -basedir /path/to/library -public-url https://books.example.com
```

On startup the log confirms each provider and the exact callback URL it expects:

```
oauth: github sign-in enabled (callback https://books.example.com/auth/github/callback)
oauth: google sign-in enabled (callback https://books.example.com/auth/google/callback)
```

Check these match what you registered. A **Continue with GitHub/Google** button then appears on the sign-in page.

If you run CalibreFast as a service, put the variables in the service's environment rather than a shell profile. For systemd, use an `EnvironmentFile=` readable only by the service user. Keep the secrets out of version control.

## Who can sign in with GitHub/Google

Accounts are matched by GitHub's or Google's permanent user ID, never by email address, so an existing account is never "found" just because the email matches. How someone gets in depends on registration:

- **Registration closed (the default):** a GitHub/Google sign-in only works for an account it's been linked to. To link one:
  1. Sign in with your username and password, or with a [one-time sign-in link](SECURITY.md#one-time-links) from an admin.
  2. Go to **Account → Linked sign-ins → Link GitHub** (or Google) and approve.
  3. From then on, **Continue with GitHub/Google** on the sign-in page works.
- **Invite links:** someone with an [invite link](SECURITY.md#one-time-links) can create their account with **Create account with GitHub/Google** directly. This works even with registration closed.
- **`-allow-registrations`:** anyone who can reach the server and signs in with GitHub/Google gets a new, regular (non-admin) account. Only do this on a trusted network; see [SECURITY.md](SECURITY.md).

People can unlink a provider from their account page, but CalibreFast won't let them remove their only way to sign in.

## Troubleshooting

| What you see | Likely cause |
|---|---|
| No GitHub/Google button on the sign-in page | Check the startup log. It says why a provider is disabled: it needs both `_CLIENT_ID` and `_CLIENT_SECRET`, plus `-public-url`. |
| GitHub: "The redirect_uri is not associated with this application" | The callback URL registered on GitHub doesn't match `<public-url>/auth/github/callback` exactly. |
| Google: "Error 400: redirect_uri_mismatch" | Same, for Google's **Authorized redirect URIs**. Check `http`/`https`, the host, the port and any trailing slash. |
| Google: "Access blocked" / "has not completed the Google verification process" | The consent screen is in *Testing* and this account isn't listed under **Test users**, or the app needs publishing. |
| Back on CalibreFast with "Sign-in expired or was started in another browser" | Sign-in has to start and finish in the same browser within 10 minutes, at the same address as `-public-url`. Opening CalibreFast at `localhost` while `-public-url` says something else causes this. |
| "No account is linked to that sign-in…" | Registration is closed and this GitHub/Google account isn't linked yet; see [above](#who-can-sign-in-with-githubgoogle). |
| "That sign-in is already linked to a different account" | That GitHub/Google account is linked to another CalibreFast user. Unlink it there first. |
| "Sign-in failed. Please try again." | The secret is wrong or was regenerated. The server log has a line starting `oauth github: token exchange:` with details. |

## Rotating or removing credentials

- **To rotate a secret:** generate a new one on GitHub or Google, update the environment variable and restart CalibreFast. Then delete the old secret on the provider's side.
- **To turn a provider off:** unset its environment variables and restart. Linked identities are kept, so turning it back on later restores those sign-ins.
