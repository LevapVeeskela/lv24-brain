// Command releasesign creates the Ed25519 release-signing key pair and signs
// or verifies release.json. It is a maintainer/CI tool and is not shipped in
// release archives.
//
//	go run ./internal/tools/releasesign keygen -out <private-key-file>
//	go run ./internal/tools/releasesign sign -in release.json -out release.json.sig
//	go run ./internal/tools/releasesign verify -in release.json -sig release.json.sig [-pub <base64>]
//
// keygen writes only the private seed to a new file and prints only the public
// key. sign reads the seed from $BRAIN_RELEASE_SIGNING_KEY and, by default,
// refuses to sign unless the derived public key is embedded in this build, so
// CI cannot publish releases that installed binaries would reject.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/levapveeskela/lv24-brain/internal/update"
)

// envSigningKey holds the base64 Ed25519 seed in CI.
const envSigningKey = "BRAIN_RELEASE_SIGNING_KEY"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: releasesign keygen|sign|verify [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout, stderr)
	case "sign":
		err = sign(args[1:], stdout, stderr, getenv)
	case "verify":
		err = verify(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "releasesign: unknown command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "releasesign %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func keygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "file to create with the base64 private seed (must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(private.Seed()) + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "private seed written to %s (store it as the %s secret, then delete the file)\n", *out, envSigningKey)
	fmt.Fprintf(stdout, "public key (add to trustedReleaseKeys in internal/update/signature.go):\n%s\n",
		base64.StdEncoding.EncodeToString(public))
	return nil
}

func sign(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "release.json to sign")
	out := fs.String("out", "", "signature file to write")
	allowUnembedded := fs.Bool("allow-unembedded", false, "sign even if the key is not embedded in this build (testing only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("-in and -out are required")
	}
	seed := getenv(envSigningKey)
	if seed == "" {
		return fmt.Errorf("%s is not set", envSigningKey)
	}
	private, err := update.ParsePrivateKey(seed)
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	signature := update.SignManifest(manifest, private)
	if !*allowUnembedded {
		embedded, err := update.DefaultTrustedKeys()
		if err != nil {
			return err
		}
		if err := update.VerifyManifestSignature(manifest, signature, embedded); err != nil {
			return fmt.Errorf("signing key is not embedded in this build (%w); add its public key to trustedReleaseKeys first", err)
		}
	}
	if err := os.WriteFile(*out, signature, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s -> %s\n", *in, *out)
	return nil
}

func verify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "release.json")
	sigPath := fs.String("sig", "", "release.json.sig")
	pub := fs.String("pub", "", "base64 public key (default: keys embedded in this build)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *sigPath == "" {
		return errors.New("-in and -sig are required")
	}
	var keys []ed25519.PublicKey
	if *pub != "" {
		key, err := update.ParsePublicKey(*pub)
		if err != nil {
			return err
		}
		keys = []ed25519.PublicKey{key}
	} else {
		embedded, err := update.DefaultTrustedKeys()
		if err != nil {
			return err
		}
		keys = embedded
	}
	manifest, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(*sigPath)
	if err != nil {
		return err
	}
	if err := update.VerifyManifestSignature(manifest, signature, keys); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signature OK for %s\n", *in)
	return nil
}
