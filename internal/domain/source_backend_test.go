package domain

import (
	"strings"
	"testing"
)

func backendDescriptor() SourceDescriptor {
	return SourceDescriptor{
		Provider:       "demo",
		DisplayName:    "Demo",
		Capabilities:   Capabilities{AuthModes: []AuthMode{AuthToken}},
		DefaultPublish: PublishShared,
	}
}

func TestDescriptorBackendsAndStreams(t *testing.T) {
	cases := []struct {
		name     string
		backends []Backend
		streams  []string
		wantErr  string
	}{
		{name: "defaults", wantErr: ""},
		{name: "native and mcp", backends: []Backend{BackendNative, BackendMCP}, streams: []string{"issues", "commits"}},
		{name: "unknown backend", backends: []Backend{"grpc"}, wantErr: "unknown backend"},
		{name: "duplicate backend", backends: []Backend{BackendMCP, BackendMCP}, wantErr: "duplicate backend"},
		{name: "bad stream", streams: []string{"bad stream"}, wantErr: "must match"},
		{name: "duplicate stream ignoring case", streams: []string{"INBOX", "inbox"}, wantErr: "duplicate stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			descriptor := backendDescriptor()
			descriptor.Backends = tc.backends
			descriptor.Streams = tc.streams
			// Act
			err := descriptor.Validate()
			// Assert
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConnectionBackendAgainstDescriptor(t *testing.T) {
	cases := []struct {
		name     string
		backends []Backend
		backend  Backend
		wantErr  string
	}{
		{name: "empty means native on a native-only source", backend: ""},
		{name: "explicit native", backend: BackendNative},
		{name: "mcp rejected on a native-only source", backend: BackendMCP, wantErr: "not supported"},
		{name: "mcp accepted when declared", backends: []Backend{BackendNative, BackendMCP}, backend: BackendMCP},
		{name: "native rejected on an mcp-only source", backends: []Backend{BackendMCP}, backend: "", wantErr: "not supported"},
		{name: "unknown backend", backend: "grpc", wantErr: "unknown backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			descriptor := backendDescriptor()
			descriptor.Backends = tc.backends
			connection := Connection{ID: "demo-1", Provider: "demo", Backend: tc.backend}
			// Act
			err := descriptor.ValidateConnection(connection)
			// Assert
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ValidateConnection() = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("ValidateConnection() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestItemBodyCap(t *testing.T) {
	// Arrange
	item := Item{
		Ref:   ItemRef{ConnectionID: "demo-1", ExternalID: "42"},
		Kind:  ItemDocument,
		Title: "Large",
		Body:  strings.Repeat("a", MaxItemBodyBytes),
	}
	// Act
	atCap := item.Validate()
	item.Body += "a"
	overCap := item.Validate()
	// Assert
	if atCap != nil {
		t.Fatalf("body at the cap: Validate() = %v, want nil", atCap)
	}
	if overCap == nil || !strings.Contains(overCap.Error(), "at most") {
		t.Fatalf("body over the cap: Validate() = %v, want size error", overCap)
	}
}

func TestCredentialKeyStaysDistinctAfterBackendFolding(t *testing.T) {
	// Arrange: backends may fold separators to '_' and ignore case.
	fold := func(name string) string {
		return strings.ToLower(strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(name))
	}
	pairs := [][2]string{{"a-b", "token"}, {"a", "btoken"}, {"a-b-c", "token"}, {"a-bc", "token"}, {"ab", "token"}}
	seen := map[string][2]string{}
	for _, pair := range pairs {
		// Act
		key, err := CredentialKey(pair[0], pair[1])
		if err != nil {
			t.Fatalf("CredentialKey(%q, %q) = %v", pair[0], pair[1], err)
		}
		folded := fold(key)
		// Assert
		if previous, ok := seen[folded]; ok {
			t.Fatalf("%v and %v fold to the same name %q", previous, pair, folded)
		}
		seen[folded] = pair
	}
}
