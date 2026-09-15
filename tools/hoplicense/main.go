// Command hoplicense is the maintainer-only tool for hop's consent-token
// gate: generate the signing keypair once, then issue a token per person
// (or entity) who has been given consent to use hop. It is never shipped —
// hop's own binary only ever verifies tokens, using the public half of the
// keypair this prints, pasted into internal/core/license.go.
//
// The private key this generates must never be committed to the repository
// or shared: anyone holding it can issue tokens on the copyright holder's
// behalf.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

type token struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp,omitempty"`
	Gov       bool   `json:"gov,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "keygen":
		keygen()
	case "issue":
		issue(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `hoplicense — issue hop consent tokens (maintainer-only, never distributed with hop)

usage:
  hoplicense keygen
      Generate a new Ed25519 keypair. Prints the public key to paste into
      internal/core/license.go (LicensePublicKeyB64), and writes the private
      key to hoplicense.key in the current directory — keep that file secret
      and out of git; it is what lets you issue tokens.

  hoplicense issue -subject "name or email" [-expires 8760h] [-gov] [-key hoplicense.key]
      Issue a signed consent token for one person or entity. Print it and
      hand it to them; they install it with:
          hop license install <token>`)
}

func keygen() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)

	const keyFile = "hoplicense.key"
	if _, err := os.Stat(keyFile); err == nil {
		fmt.Fprintf(os.Stderr, "%s already exists — refusing to overwrite an existing signing key\n", keyFile)
		os.Exit(1)
	}
	must(os.WriteFile(keyFile, priv, 0o600))

	fmt.Println("private key written to", keyFile)
	fmt.Println("keep it secret, keep it out of git — anyone with it can issue tokens as you")
	fmt.Println()
	fmt.Println("paste this into internal/core/license.go as LicensePublicKeyB64:")
	fmt.Println()
	fmt.Println("    " + base64.StdEncoding.EncodeToString(pub))
}

func issue(args []string) {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	subject := fs.String("subject", "", "who consent is being granted to (name, email, org)")
	expires := fs.Duration("expires", 0, "how long the token is valid for, e.g. 8760h (0 = never expires)")
	gov := fs.Bool("gov", false, "issue under the government-entity exemption")
	keyFile := fs.String("key", "hoplicense.key", "path to the private key from `hoplicense keygen`")
	_ = fs.Parse(args)

	if *subject == "" {
		fmt.Fprintln(os.Stderr, "issue: -subject is required")
		os.Exit(2)
	}

	priv, err := os.ReadFile(*keyFile)
	must(err)
	if len(priv) != ed25519.PrivateKeySize {
		fmt.Fprintf(os.Stderr, "%s is not a valid Ed25519 private key (run `hoplicense keygen` first)\n", *keyFile)
		os.Exit(1)
	}

	tok := token{
		Subject:  *subject,
		IssuedAt: time.Now().Unix(),
		Gov:      *gov,
	}
	if *expires > 0 {
		tok.ExpiresAt = time.Now().Add(*expires).Unix()
	}

	payload, err := json.Marshal(tok)
	must(err)

	sig := ed25519.Sign(ed25519.PrivateKey(priv), payload)
	raw := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)

	fmt.Println(raw)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
