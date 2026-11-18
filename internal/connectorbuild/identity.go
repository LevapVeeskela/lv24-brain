package connectorbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// AccountField is the credential-store field that holds a connection account.
// It is not a descriptor secret: the account is a person's identity (a mailbox
// or workspace login), kept beside tokens under conn/<id>/account.
const AccountField = "account"

// IdentityMarkerKey is the JSON key of the marker stored in the catalogue
// instead of an identity value:
//
//	{"$keychain":"username","fingerprint":"..."}
//
// The fingerprint is a truncated SHA-256 of the normalized value. It keeps
// Connection.Identity() unique per account without publishing the address, and
// it is identical on every machine, so a raw legacy value and its migrated
// form compare equal.
const IdentityMarkerKey = "$keychain"

const (
	accountFingerprintBytes = 8
	accountFingerprintHex   = accountFingerprintBytes * 2
)

// identitySettings declares the non-secret provider settings that carry a
// person's identity. Values in this list are stored in the secret store like
// the account, never in the git catalogue. Providers without an identity field
// (notion and slack use tokens plus container IDs) need no entry.
var identitySettings = map[string][]string{
	imap.Name: {"username"},
}

// IdentityMarker is the value stored in Connection.Account or in a declared
// identity setting. Keychain names the credential-store field; Fingerprint
// binds the marker to the value it replaces.
type IdentityMarker struct {
	Keychain    string `json:"$keychain"`
	Fingerprint string `json:"fingerprint"`
}

// IdentitySettingKeys returns the provider's declared identity settings.
func IdentitySettingKeys(provider string) []string {
	return append([]string(nil), identitySettings[strings.ToLower(strings.TrimSpace(provider))]...)
}

// IsIdentitySetting reports whether key is a declared identity setting of
// provider.
func IsIdentitySetting(provider, key string) bool {
	for _, field := range IdentitySettingKeys(provider) {
		if field == key {
			return true
		}
	}
	return false
}

// IsIdentityMarker reports whether value is a marker for any field.
func IsIdentityMarker(value string) bool {
	_, ok := decodeIdentityMarker(value)
	return ok
}

// IsAccountMarker reports whether account is the marker of Connection.Account.
func IsAccountMarker(account string) bool {
	marker, ok := decodeIdentityMarker(account)
	return ok && marker.Keychain == AccountField
}

// IsSettingMarker reports whether value is a marker for the declared setting.
func IsSettingMarker(provider, field, value string) bool {
	marker, ok := decodeIdentityMarker(value)
	return ok && marker.Keychain == field && IsIdentitySetting(provider, field)
}

// IdentityMarkerMatches reports whether marker was produced from raw.
func IdentityMarkerMatches(marker, raw string) bool {
	decoded, ok := decodeIdentityMarker(marker)
	return ok && decoded.Matches(raw)
}

// Matches reports whether raw produced this marker.
func (m IdentityMarker) Matches(raw string) bool {
	return m.Fingerprint == accountFingerprint(raw)
}

// AccountMarkerMatches reports whether the account marker was produced from
// raw. It lets `connection secret set <id> account` repair a missing secret
// without silently changing the connection identity.
func AccountMarkerMatches(account, raw string) bool {
	return IsAccountMarker(account) && IdentityMarkerMatches(account, raw)
}

// IdentitySettingMarkerMatches reports whether the setting marker was produced
// from raw, so a repair cannot silently change the value a connection reads.
func IdentitySettingMarkerMatches(provider, field, marker, raw string) bool {
	return IsSettingMarker(provider, field, marker) && IdentityMarkerMatches(marker, raw)
}

// DescribeAccount returns a safe label for terminal and JSON output. A marker
// is never echoed as if it were a mailbox name.
func DescribeAccount(account string) string {
	if !IsAccountMarker(account) {
		return account
	}
	return "<keychain>"
}

// IdentityKey is Connection.Identity() with a raw account normalized to the
// persisted marker form. Create and migration compare this key so a migrated
// connection is not duplicated by its legacy raw account (and vice versa).
func IdentityKey(connection domain.Connection) string {
	if strings.TrimSpace(connection.Account) != "" && !IsAccountMarker(connection.Account) {
		connection.Account = encodeIdentityMarker(AccountField, connection.Account)
	}
	return connection.Identity()
}

// ProtectIdentity stores every raw identity value of the connection (the
// account and its declared identity settings) in the secret store under
// conn/<id>/<field>, reads each back, and returns a copy with markers. The
// input is not mutated. Empty values and existing markers are left unchanged,
// so retries and already-migrated connections are safe.
//
// Callers must persist the returned copy. Errors never contain the values or
// backend output.
func ProtectIdentity(ctx context.Context, connection domain.Connection, credentials ports.CredentialStore) (domain.Connection, error) {
	protected := connection
	if len(connection.Settings) > 0 {
		protected.Settings = make(map[string]string, len(connection.Settings))
		for key, value := range connection.Settings {
			protected.Settings[key] = value
		}
	}
	account, err := ProtectAccount(ctx, connection.ID, connection.Account, credentials)
	if err != nil {
		return domain.Connection{}, err
	}
	protected.Account = account
	for _, field := range IdentitySettingKeys(connection.Provider) {
		value, ok := protected.Settings[field]
		if !ok || strings.TrimSpace(value) == "" || IsSettingMarker(connection.Provider, field, value) {
			continue
		}
		marker, err := protectIdentityValue(ctx, connection.ID, field, value, credentials)
		if err != nil {
			return domain.Connection{}, err
		}
		protected.Settings[field] = marker
	}
	return protected, nil
}

// ProtectAccount stores a raw account in the secret store and returns the
// marker to persist in Connection.Account.
func ProtectAccount(ctx context.Context, connectionID, account string, credentials ports.CredentialStore) (string, error) {
	if strings.TrimSpace(account) == "" || IsAccountMarker(account) {
		return account, nil
	}
	return protectIdentityValue(ctx, connectionID, AccountField, account, credentials)
}

// ResolveIdentity returns a connection with its account and declared identity
// settings restored from the secret store. Call it lazily, when a connector or
// an explicit check actually reads the identity, never on a startup or
// scheduled path for a whole catalogue.
//
// A legacy raw value is returned unchanged; that fallback stays forever. A
// marker without a usable secret returns a clear domain.ErrNotFound error that
// names the missing credential key and the repair command.
func ResolveIdentity(ctx context.Context, connection domain.Connection, credentials ports.CredentialProvider) (domain.Connection, error) {
	resolved, err := ResolveAccount(ctx, connection, credentials)
	if err != nil {
		return domain.Connection{}, err
	}
	return ResolveIdentitySettings(ctx, resolved, credentials)
}

// ResolveIdentitySettings resolves only the provider's declared identity
// settings, leaving the account untouched. Registry.Build uses it so a run
// reads the mailbox login without touching the account of every connection.
func ResolveIdentitySettings(ctx context.Context, connection domain.Connection, credentials ports.CredentialProvider) (domain.Connection, error) {
	resolved := connection
	if len(connection.Settings) > 0 {
		resolved.Settings = make(map[string]string, len(connection.Settings))
		for key, value := range connection.Settings {
			resolved.Settings[key] = value
		}
	}
	for _, field := range IdentitySettingKeys(connection.Provider) {
		value, ok := resolved.Settings[field]
		if !ok || !IsSettingMarker(connection.Provider, field, value) {
			continue
		}
		marker, _ := decodeIdentityMarker(value)
		raw, err := readIdentitySecret(ctx, connection.ID, field, marker, credentials)
		if err != nil {
			return domain.Connection{}, err
		}
		resolved.Settings[field] = raw
	}
	return resolved, nil
}

// ResolveAccount returns a connection with its account restored from the
// secret store. A legacy raw account is returned unchanged.
func ResolveAccount(ctx context.Context, connection domain.Connection, credentials ports.CredentialProvider) (domain.Connection, error) {
	if !IsAccountMarker(connection.Account) {
		return connection, nil
	}
	marker, _ := decodeIdentityMarker(connection.Account)
	raw, err := readIdentitySecret(ctx, connection.ID, AccountField, marker, credentials)
	if err != nil {
		return domain.Connection{}, err
	}
	connection.Account = raw
	return connection, nil
}

// IdentityProblem classifies one broken identity marker. IdentityIssues never
// fails validation or a catalogue read: doctor and diagnostics render these
// rows instead.
type IdentityProblem string

const (
	IdentityProblemMissing    IdentityProblem = "missing-secret"
	IdentityProblemUnreadable IdentityProblem = "unreadable-secret"
	IdentityProblemMismatch   IdentityProblem = "mismatched-secret"
)

// IdentityIssue is one protected identity value that cannot be resolved.
type IdentityIssue struct {
	ConnectionID string          `json:"connectionId"`
	Field        string          `json:"field"`
	Key          string          `json:"key"`
	Problem      IdentityProblem `json:"problem"`
}

// IdentityIssues reports markers whose secret is missing, unreadable or no
// longer matches the marker. It is read-only and never returns an error, so
// doctor can list broken identities without failing the whole catalogue.
func IdentityIssues(ctx context.Context, connections []domain.Connection, credentials ports.CredentialProvider) []IdentityIssue {
	issues := []IdentityIssue{}
	for _, connection := range connections {
		for _, field := range IdentityFields(connection) {
			marker, ok := IdentityMarkerFor(connection, field)
			if !ok {
				continue
			}
			key, err := domain.CredentialKey(connection.ID, field)
			if err != nil {
				continue
			}
			issue := IdentityIssue{ConnectionID: connection.ID, Field: field, Key: key}
			switch {
			case credentials == nil:
				issue.Problem = IdentityProblemUnreadable
			default:
				value, err := credentials.Get(ctx, connection.ID, field)
				switch {
				case errors.Is(err, domain.ErrNotFound):
					issue.Problem = IdentityProblemMissing
				case err != nil:
					issue.Problem = IdentityProblemUnreadable
				case strings.TrimSpace(value) == "":
					issue.Problem = IdentityProblemMissing
				case marker.Fingerprint != accountFingerprint(value):
					issue.Problem = IdentityProblemMismatch
				}
			}
			if issue.Problem != "" {
				issues = append(issues, issue)
			}
		}
	}
	return issues
}

// IdentityFields lists the identity fields a connection actually stores, in a
// stable order: the account first, then the provider's declared settings.
func IdentityFields(connection domain.Connection) []string {
	fields := make([]string, 0, 1+len(IdentitySettingKeys(connection.Provider)))
	if strings.TrimSpace(connection.Account) != "" {
		fields = append(fields, AccountField)
	}
	for _, field := range IdentitySettingKeys(connection.Provider) {
		if strings.TrimSpace(connection.Settings[field]) != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

// IdentityMarkerFor returns the marker stored for field, if any. field is
// AccountField or a declared identity setting of the connection's provider.
func IdentityMarkerFor(connection domain.Connection, field string) (IdentityMarker, bool) {
	if field == AccountField {
		if !IsAccountMarker(connection.Account) {
			return IdentityMarker{}, false
		}
		return decodeIdentityMarker(connection.Account)
	}
	if !IsSettingMarker(connection.Provider, field, connection.Settings[field]) {
		return IdentityMarker{}, false
	}
	return decodeIdentityMarker(connection.Settings[field])
}

func protectIdentityValue(ctx context.Context, connectionID, field, value string, credentials ports.CredentialStore) (string, error) {
	if credentials == nil {
		return "", domain.NewValidationError("identity.credentials", "is required to store a connection identity value")
	}
	if _, err := domain.CredentialKey(connectionID, field); err != nil {
		return "", err
	}
	if err := credentials.Set(ctx, connectionID, field, value); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("connection %q: could not store %s in the secret store; check the secret backend", connectionID, field)
	}
	stored, err := credentials.Get(ctx, connectionID, field)
	if err != nil || stored != value {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("connection %q: %s could not be verified in the secret store; the catalogue was not changed", connectionID, field)
	}
	return encodeIdentityMarker(field, value), nil
}

func readIdentitySecret(ctx context.Context, connectionID, field string, marker IdentityMarker, credentials ports.CredentialProvider) (string, error) {
	key, err := domain.CredentialKey(connectionID, field)
	if err != nil {
		return "", err
	}
	if credentials == nil {
		return "", domain.NewValidationError("identity.credentials", "is required to read "+field+" of connection "+connectionID)
	}
	value, err := credentials.Get(ctx, connectionID, field)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(err, domain.ErrNotFound) {
			return "", missingIdentityError(connectionID, field, key)
		}
		return "", fmt.Errorf("connection %q: the %s secret %q could not be read from the secret store", connectionID, field, key)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", missingIdentityError(connectionID, field, key)
	}
	if marker.Fingerprint != accountFingerprint(value) {
		return "", fmt.Errorf("%w: connection %q: the %s secret %q does not match the stored marker; set it again with: brain connection secret set %s %s", domain.ErrValidation, connectionID, field, key, connectionID, field)
	}
	return value, nil
}

func missingIdentityError(connectionID, field, key string) error {
	return fmt.Errorf("%w: connection %q stores its %s in the secret store as %q, but the secret is missing; restore it with: brain connection secret set %s %s", domain.ErrNotFound, connectionID, field, key, connectionID, field)
}

func encodeIdentityMarker(field, value string) string {
	// The key is a validated config key and the digest is lowercase hex, so the
	// literal is valid JSON without escaping.
	return `{"` + IdentityMarkerKey + `":"` + field + `","fingerprint":"` + accountFingerprint(value) + `"}`
}

func decodeIdentityMarker(value string) (IdentityMarker, bool) {
	raw := strings.TrimSpace(value)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return IdentityMarker{}, false
	}
	var marker IdentityMarker
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return IdentityMarker{}, false
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return IdentityMarker{}, false
	}
	if domain.ValidateConfigKey("identity.marker", marker.Keychain) != nil || !validAccountFingerprint(marker.Fingerprint) {
		return IdentityMarker{}, false
	}
	return marker, true
}

func accountFingerprint(value string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(value))))
	return hex.EncodeToString(sum[:accountFingerprintBytes])
}

func validAccountFingerprint(value string) bool {
	if len(value) != accountFingerprintHex {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
