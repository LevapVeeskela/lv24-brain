package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/sourceoauth"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/oauthlogin"
)

// oauthClientIDEnv supplies the public client ID when --client-id is omitted.
// Client IDs are not secrets, but they are deployment-specific.
const oauthClientIDEnv = "BRAIN_OAUTH_CLIENT_ID"

// connectionAuth runs a PKCE loopback or device flow for one connection and
// stores the resulting bundle only in the local secret store.
func connectionAuth(ctx context.Context, runtime *bootstrap.ConnectionRuntime, id, clientID, scopesRaw string, remove bool, stdout, stderr io.Writer) int {
	connection, err := runtime.Connections.Get(ctx, id)
	if err != nil {
		return reportError(stderr, "connection auth", err)
	}
	store := &oauthlogin.SessionStore{Secrets: runtime.Secrets}
	if remove {
		if err := store.Delete(ctx, id); err != nil {
			return reportError(stderr, "connection auth", err)
		}
		fmt.Fprintf(stdout, "OAuth session removed for %s\n", id)
		return 0
	}
	provider, ok := sourceoauth.Providers()[connection.Provider]
	if !ok {
		return connectionValidationError(stderr, "auth", fmt.Errorf("%s does not have built-in OAuth endpoints yet; use a token or pass a configured provider", connection.Provider))
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = strings.TrimSpace(os.Getenv(oauthClientIDEnv))
	}
	if clientID == "" {
		return connectionValidationError(stderr, "auth", fmt.Errorf("--client-id or %s is required", oauthClientIDEnv))
	}
	scopes := splitOAuthScopes(scopesRaw)
	if len(scopes) == 0 {
		scopes = append([]string(nil), provider.DefaultScopes...)
	}

	var bundle sourceoauth.Bundle
	if provider.Device {
		flow := sourceoauth.DeviceFlow{Provider: provider, ClientID: clientID, Scopes: scopes}
		bundle, err = flow.Device(ctx, func(prompt sourceoauth.DevicePrompt) error {
			fmt.Fprintf(stdout, "Open %s and enter code %s\n", prompt.VerificationURI, prompt.UserCode)
			if prompt.VerificationURIComplete != "" {
				fmt.Fprintf(stdout, "Or open %s\n", prompt.VerificationURIComplete)
			}
			return nil
		})
	} else {
		flow := sourceoauth.NewFlow(provider, clientID)
		flow.Scopes = scopes
		bundle, err = flow.Authorize(ctx, func(authURL string) error {
			fmt.Fprintln(stdout, "Open this URL in a browser to authorize:")
			fmt.Fprintln(stdout, authURL)
			_ = openBrowser(authURL)
			return nil
		})
	}
	if err != nil {
		return reportError(stderr, "connection auth", err)
	}
	session := oauthlogin.Session{
		Provider: connection.Provider,
		ClientID: clientID,
		Scopes:   scopes,
		Bundle:   bundle,
	}
	if err := store.Save(ctx, id, session); err != nil {
		return reportError(stderr, "connection auth", err)
	}
	fmt.Fprintf(stdout, "OAuth session stored for %s (%s)\n", id, connection.Provider)
	return 0
}

// splitOAuthScopes accepts comma or whitespace separated scopes.
func splitOAuthScopes(raw string) []string {
	var scopes []string
	for _, scope := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}) {
		if scope != "" {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

// openBrowser starts the system browser without a shell. A failure is ignored:
// the caller has already printed the URL and the loopback flow keeps waiting.
func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
