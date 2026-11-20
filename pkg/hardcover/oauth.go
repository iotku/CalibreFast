package hardcover

// Signing a user in to Hardcover with OAuth's Device Authorization Grant
// (RFC 8628): we ask for a code, the user enters it at hardcover.app/link
// and approves, and our polling gets an access and a refresh token. There
// is no redirect, so it works for a server at any address, and the client
// is public (no secret; Hardcover requires none with the device grant).
//
// Access tokens last a week; refresh tokens six months, and they rotate:
// each refresh answers a new one and spends the old. A refresh token used
// twice revokes the whole chain, so the caller must keep the newest before
// using the access token it came with.
//
// No error here holds a token, a code or a URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Hardcover's OAuth endpoints (also in its discovery document,
// https://api.hardcover.app/.well-known/oauth-authorization-server).
const (
	DefaultDeviceURL = "https://api.hardcover.app/oauth2/device"
	DefaultTokenURL  = "https://api.hardcover.app/oauth2/token"
	DefaultRevokeURL = "https://api.hardcover.app/oauth2/revoke"
	// VerificationURL is where the user enters the code.
	VerificationURL = "https://hardcover.app/link"
)

// SyncScopes are the scopes reading sync asks for: who the user is, their
// library (to find a book's entry) and writing to it, and the catalog (to
// find a book and its edition).
const SyncScopes = "read:me read:library write:library read:catalog"

// OAuth signs users in. Zero URLs are Hardcover's.
type OAuth struct {
	ClientID                       string
	DeviceURL, TokenURL, RevokeURL string
	UserAgent                      string
	HTTPClient                     *http.Client
	Now                            func() time.Time
}

// DeviceAuth is a started sign-in: show the user UserCode and
// VerificationURI, and poll with DeviceCode every Interval until Expires.
type DeviceAuth struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string // always VerificationURL
	// VerificationURIComplete opens VerificationURI with the code filled
	// in; "" unless Hardcover gave one on https://hardcover.app/link.
	VerificationURIComplete string
	Expires                 time.Time
	Interval                time.Duration
}

// Token is a signed-in user's access, as Hardcover answered it.
type Token struct {
	Access, Refresh string
	Expires         time.Time // of Access
	Scope           string
}

// Errors from polling and refreshing (RFC 8628 §3.5, RFC 6749 §5.2).
var (
	ErrAuthorizationPending = errors.New("waiting for the code to be entered on Hardcover")
	ErrSlowDown             = errors.New("Hardcover asked us to poll more slowly")
	ErrAccessDenied         = errors.New("the sign-in was declined on Hardcover")
	ErrExpiredCode          = errors.New("the code expired before it was entered on Hardcover")
	// ErrInvalidGrant: the refresh token is spent, expired or revoked; the
	// user has to sign in again.
	ErrInvalidGrant = errors.New("Hardcover no longer accepts this sign-in")
)

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OAuth) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// form posts form to u and decodes a 200 answer into out. Other answers
// come back as the OAuth error code (status 0: unreachable).
func (o *OAuth) form(ctx context.Context, u string, form url.Values, out any) (status int, code string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", o.UserAgent)
	resp, err := o.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		return 0, "", errors.New("couldn't reach Hardcover")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, "", errors.New("reading Hardcover's answer failed")
	}
	if resp.StatusCode != http.StatusOK {
		var e oauthError
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, e.Error, nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, "", errors.New("Hardcover's answer isn't JSON")
		}
	}
	return resp.StatusCode, "", nil
}

// StartDevice starts a sign-in for scope.
func (o *OAuth) StartDevice(ctx context.Context, scope string) (*DeviceAuth, error) {
	if o.ClientID == "" {
		return nil, errors.New("no Hardcover OAuth client is set up")
	}
	var r struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Complete   string `json:"verification_uri_complete"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	status, code, err := o.form(ctx, orDefault(o.DeviceURL, DefaultDeviceURL),
		url.Values{"client_id": {o.ClientID}, "scope": {scope}}, &r)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Hardcover refused to start a sign-in (HTTP %d %s)", status, clip(code))
	}
	if r.DeviceCode == "" || !validUserCode(r.UserCode) {
		return nil, errors.New("Hardcover's answer has no usable code")
	}
	d := &DeviceAuth{
		DeviceCode:      r.DeviceCode,
		UserCode:        r.UserCode,
		VerificationURI: VerificationURL,
		Expires:         o.now().Add(time.Duration(min(max(r.ExpiresIn, 60), 3600)) * time.Second),
		Interval:        time.Duration(min(max(r.Interval, 5), 60)) * time.Second,
	}
	if verificationLink(r.Complete) {
		d.VerificationURIComplete = r.Complete
	}
	return d, nil
}

// verificationLink: u is Hardcover's own code-entry page (so it's safe to
// show as a link): https://hardcover.app/link, with only a query.
func verificationLink(u string) bool {
	p, err := url.Parse(u)
	return err == nil && len(u) <= 300 && p.Scheme == "https" && p.Host == "hardcover.app" && p.User == nil &&
		(p.Path == "/link" || p.Path == "/link/") && p.Fragment == ""
}

// validUserCode: the code shown to the user looks like one ("ABCD-1234"):
// short, and only letters, digits and dashes.
func validUserCode(s string) bool {
	if len(s) < 4 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (o *OAuth) token(ctx context.Context, form url.Values) (*Token, string, error) {
	var r tokenAnswer
	status, code, err := o.form(ctx, orDefault(o.TokenURL, DefaultTokenURL), form, &r)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		if code == "" {
			code = fmt.Sprintf("http_%d", status)
		}
		return nil, code, nil
	}
	if r.AccessToken == "" || r.RefreshToken == "" {
		return nil, "", errors.New("Hardcover's answer has no token")
	}
	exp := r.ExpiresIn
	if exp <= 0 {
		exp = 7 * 24 * 3600
	}
	return &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Scope: r.Scope,
		Expires: o.now().Add(time.Duration(exp) * time.Second)}, "", nil
}

// PollDevice asks once whether the user has approved d. Until they have,
// it answers ErrAuthorizationPending (or ErrSlowDown: add 5 s to the
// interval); ErrAccessDenied and ErrExpiredCode end the sign-in.
func (o *OAuth) PollDevice(ctx context.Context, deviceCode string) (*Token, error) {
	t, code, err := o.token(ctx, url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {o.ClientID},
	})
	switch {
	case err != nil:
		return nil, err
	case t != nil:
		return t, nil
	case code == "authorization_pending":
		return nil, ErrAuthorizationPending
	case code == "slow_down":
		return nil, ErrSlowDown
	case code == "access_denied":
		return nil, ErrAccessDenied
	case code == "expired_token":
		return nil, ErrExpiredCode
	}
	return nil, fmt.Errorf("Hardcover refused the sign-in (%s)", clip(code))
}

// Refresh trades a refresh token for a new pair. The one passed is spent
// whether or not the answer arrives: keep the new one before anything else.
func (o *OAuth) Refresh(ctx context.Context, refresh string) (*Token, error) {
	t, code, err := o.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {o.ClientID},
	})
	switch {
	case err != nil:
		return nil, err
	case t != nil:
		return t, nil
	case code == "invalid_grant" || code == "unauthorized_client" || code == "invalid_client":
		return nil, ErrInvalidGrant
	}
	return nil, fmt.Errorf("Hardcover couldn't renew the sign-in (%s)", clip(code))
}

// Revoke ends a sign-in. Revoking the refresh token ends its whole chain.
func (o *OAuth) Revoke(ctx context.Context, refresh string) error {
	status, code, err := o.form(ctx, orDefault(o.RevokeURL, DefaultRevokeURL), url.Values{
		"token":           {refresh},
		"token_type_hint": {"refresh_token"},
		"client_id":       {o.ClientID},
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Hardcover couldn't revoke the sign-in (HTTP %d %s)", status, clip(code))
	}
	return nil
}
