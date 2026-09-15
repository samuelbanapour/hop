package core

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LicenseServiceURL is where `hop license accept` requests a consent token
// from — the self-service counterpart to a manually issued one. Empty by
// default; override with HOP_LICENSE_SERVICE_URL, or set a compiled-in
// default here once the service (see service/license) is deployed.
const licenseServiceURLDefault = ""

// LicenseServiceURL resolves the configured service base URL, if any.
func LicenseServiceURL() string {
	if v := os.Getenv("HOP_LICENSE_SERVICE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return licenseServiceURLDefault
}

// LicensePublicKeyB64 is the Ed25519 public key hop verifies consent tokens
// against, standard-base64-encoded. The matching private key is held only
// by the copyright holder and never checked into this repository — holding
// it is what lets someone issue a token, which is how consent is granted
// (see tools/hoplicense).
//
// An empty key means no token can ever verify, which is the safe default
// for a checkout that hasn't been configured to enforce the gate.
const LicensePublicKeyB64 = "PWPCiBv9tlW3f3wsAcBnVYQ4cNCbDuembnq0LjipLUE="

// LicenseToken is what a valid consent token decodes to.
type LicenseToken struct {
	Subject   string `json:"sub"`           // who consent was granted to
	IssuedAt  int64  `json:"iat"`           // unix seconds
	ExpiresAt int64  `json:"exp,omitempty"` // unix seconds; 0 = never expires
	Gov       bool   `json:"gov,omitempty"` // issued under the government exemption
}

// ParseLicenseToken verifies a token's signature against the embedded
// public key and checks its expiry. raw is "<base64url payload>.<base64url
// signature>", with no padding — see tools/hoplicense/main.go, which is the
// only thing that ever produces one.
func ParseLicenseToken(raw string) (*LicenseToken, error) {
	pub, err := licensePublicKey()
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(strings.TrimSpace(raw), ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed consent token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed consent token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed consent token")
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, errors.New("consent token signature does not verify")
	}

	var tok LicenseToken
	if err := json.Unmarshal(payload, &tok); err != nil {
		return nil, errors.New("malformed consent token")
	}
	if tok.ExpiresAt != 0 && time.Now().Unix() > tok.ExpiresAt {
		return nil, fmt.Errorf("consent token for %q expired %s", tok.Subject, time.Unix(tok.ExpiresAt, 0).Format("2006-01-02"))
	}
	return &tok, nil
}

func licensePublicKey() (ed25519.PublicKey, error) {
	if LicensePublicKeyB64 == "" {
		return nil, errors.New("hop was not built with a consent-token public key configured")
	}
	pub, err := base64.StdEncoding.DecodeString(LicensePublicKeyB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("hop's built-in consent-token public key is invalid")
	}
	return ed25519.PublicKey(pub), nil
}

// LicenseTokenPath is where an installed consent token lives on disk, aside
// from the HOP_LICENSE_TOKEN environment variable.
func (l *Layout) LicenseTokenPath() string { return filepath.Join(l.Root, "license.token") }

// LoadLicenseToken finds and verifies whatever consent token is available:
// HOP_LICENSE_TOKEN first (handy for CI, containers, or trying a token
// without installing it), then the installed token file.
func LoadLicenseToken(l *Layout) (*LicenseToken, error) {
	if raw := os.Getenv("HOP_LICENSE_TOKEN"); raw != "" {
		return ParseLicenseToken(raw)
	}
	b, err := os.ReadFile(l.LicenseTokenPath())
	if err != nil {
		return nil, errors.New("no consent token installed")
	}
	return ParseLicenseToken(string(b))
}

// InstallLicenseToken verifies raw and, if valid, writes it to
// LicenseTokenPath so future runs pick it up without HOP_LICENSE_TOKEN set.
func InstallLicenseToken(l *Layout, raw string) (*LicenseToken, error) {
	tok, err := ParseLicenseToken(raw)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(l.LicenseTokenPath(), []byte(strings.TrimSpace(raw)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", l.LicenseTokenPath(), err)
	}
	return tok, nil
}

// LicenseTerms is what the self-service license service's GET /terms
// returns: the exact text an acceptance record is hashed against.
type LicenseTerms struct {
	Version string `json:"version"`
	Text    string `json:"text"`
	Hash    string `json:"hash"`
}

// LicenseAcceptResult is what POST /accept returns.
type LicenseAcceptResult struct {
	RequestID string `json:"request_id"`
	IsGov     bool   `json:"is_gov"`
}

// LicenseStatus is what GET /status returns. It never carries the token
// itself — that is only ever disclosed once, through the service's /redeem
// webpage, not through any API this client calls.
type LicenseStatus struct {
	Status string `json:"status"` // "pending", "verified", or "redeemed"
	IsGov  bool   `json:"is_gov"`
}

func licenseGet(ctx context.Context, client *http.Client, path string, out any) error {
	base := LicenseServiceURL()
	if base == "" {
		return errors.New("no license service configured (set HOP_LICENSE_SERVICE_URL)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	return doLicenseRequest(client, req, out)
}

// FetchLicenseTerms fetches the terms text to show before accepting.
func FetchLicenseTerms(ctx context.Context, client *http.Client) (*LicenseTerms, error) {
	var t LicenseTerms
	if err := licenseGet(ctx, client, "/terms", &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// SubmitLicenseAcceptance records that name/email agreed to the terms and
// triggers a verification email. It does not return a token — see
// LicenseAcceptResult and the service's /redeem flow.
func SubmitLicenseAcceptance(ctx context.Context, client *http.Client, name, email string) (*LicenseAcceptResult, error) {
	base := LicenseServiceURL()
	if base == "" {
		return nil, errors.New("no license service configured (set HOP_LICENSE_SERVICE_URL)")
	}
	body, err := json.Marshal(map[string]string{"name": name, "email": email})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/accept", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	var res LicenseAcceptResult
	if err := doLicenseRequest(client, req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CheckLicenseStatus polls how far a request_id has gotten.
func CheckLicenseStatus(ctx context.Context, client *http.Client, requestID string) (*LicenseStatus, error) {
	var s LicenseStatus
	if err := licenseGet(ctx, client, "/status?req="+requestID, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func doLicenseRequest(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("license service returned %s", resp.Status)
	}
	return json.Unmarshal(b, out)
}
