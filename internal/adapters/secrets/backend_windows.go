//go:build windows

package secrets

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"unsafe"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	credentialTargetPrefix  = keychainService + "/"
)

var (
	advapi32          = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW     = advapi32.NewProc("CredReadW")
	procCredWriteW    = advapi32.NewProc("CredWriteW")
	procCredDeleteW   = advapi32.NewProc("CredDeleteW")
	procCredEnumerate = advapi32.NewProc("CredEnumerateW")
	procCredFree      = advapi32.NewProc("CredFree")
)

// credentialW mirrors CREDENTIALW. Windows Credential Manager encrypts the
// credential blob with DPAPI for the current user; this package never writes
// the blob to a file.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type platformBackend struct{}

func newPlatformBackend() backend {
	return platformBackend{}
}

func targetName(key string) string {
	return credentialTargetPrefix + key
}

func (platformBackend) Get(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return "", fmt.Errorf("encode credential target: %w", err)
	}
	var cred *credentialW
	r1, _, callErr := procCredReadW.Call(
		uintptr(unsafe.Pointer(name)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&cred)),
	)
	runtime.KeepAlive(name)
	if r1 == 0 {
		return "", mapCredentialError("read credential", key, callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred == nil || cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return "", notFound(key)
	}
	blob := unsafe.Slice(cred.CredentialBlob, int(cred.CredentialBlobSize))
	return string(blob), nil
}

func (platformBackend) Set(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return fmt.Errorf("encode credential target: %w", err)
	}
	userName, err := windows.UTF16PtrFromString(keychainService)
	if err != nil {
		return fmt.Errorf("encode credential user: %w", err)
	}
	blob := []byte(value)
	if len(blob) == 0 {
		return domain.NewValidationError("secret.value", "must not be empty")
	}
	cred := credentialW{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
		UserName:           userName,
	}
	r1, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	runtime.KeepAlive(cred)
	runtime.KeepAlive(target)
	runtime.KeepAlive(userName)
	runtime.KeepAlive(blob)
	if r1 == 0 {
		return mapCredentialError("write credential", key, callErr)
	}
	return nil
}

func (platformBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return fmt.Errorf("encode credential target: %w", err)
	}
	r1, _, callErr := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(credTypeGeneric),
		0,
	)
	runtime.KeepAlive(target)
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil
		}
		return mapCredentialError("delete credential", key, callErr)
	}
	return nil
}

func (platformBackend) Names(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filter, err := windows.UTF16PtrFromString(credentialTargetPrefix + "*")
	if err != nil {
		return nil, fmt.Errorf("encode credential filter: %w", err)
	}
	var count uint32
	var entries **credentialW
	r1, _, callErr := procCredEnumerate.Call(
		uintptr(unsafe.Pointer(filter)),
		0,
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&entries)),
	)
	runtime.KeepAlive(filter)
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return []string{}, nil
		}
		return nil, mapCredentialError("enumerate credentials", "", callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(entries)))
	if entries == nil || count == 0 {
		return []string{}, nil
	}

	credentials := unsafe.Slice(entries, int(count))
	names := make([]string, 0, len(credentials))
	for _, entry := range credentials {
		if entry == nil || entry.TargetName == nil {
			continue
		}
		target := windows.UTF16PtrToString(entry.TargetName)
		name, ok := strings.CutPrefix(target, credentialTargetPrefix)
		if !ok || name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func mapCredentialError(operation, key string, err error) error {
	switch {
	case errors.Is(err, windows.ERROR_NOT_FOUND):
		return notFound(key)
	case errors.Is(err, windows.ERROR_NO_SUCH_LOGON_SESSION), errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return unavailable(operation + " denied by the Windows secret store")
	default:
		return fmt.Errorf("%s: %w", operation, err)
	}
}
