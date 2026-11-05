package update

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

func sha256Writer() hash.Hash { return sha256.New() }

func ioMultiWriter(writers ...io.Writer) io.Writer {
	return io.MultiWriter(writers...)
}

func copyLimited(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("artifact exceeds %d bytes", limit)
	}
	return written, nil
}

func hexSum(hasher hash.Hash) string {
	return hex.EncodeToString(hasher.Sum(nil))
}

func equalSHA256(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
