package core

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
