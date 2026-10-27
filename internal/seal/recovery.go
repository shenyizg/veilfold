package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
)

// A recovery key is 128 random bits followed by a 32-bit checksum, written
// as 32 characters of Crockford's base32 in groups of four, for example
//
//	7KQ4-M2ZD-0F9X-HV3S-PW8N-C1TE-RY6B-AJ5G
//
// The alphabet avoids easily confused letters, input is case-insensitive and
// the checksum catches typing mistakes before any decryption is attempted.

const (
	recoveryKeySize      = 16
	recoveryChecksumSize = 4
	crockford            = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// ErrRecoveryKeyFormat is returned for malformed or mistyped recovery keys.
var ErrRecoveryKeyFormat = errors.New("invalid recovery key: check for typing mistakes")

var recoveryEncoding = base32.NewEncoding(crockford).WithPadding(base32.NoPadding)

func recoveryChecksum(key []byte) []byte {
	h := sha256.Sum256(append([]byte("veilfold recovery key v1"), key...))
	return h[:recoveryChecksumSize]
}

// NewRecoveryKey returns fresh recovery key material and its printable form.
func NewRecoveryKey() (key []byte, text string) {
	key = Random(recoveryKeySize)
	raw := append(append([]byte(nil), key...), recoveryChecksum(key)...)
	enc := recoveryEncoding.EncodeToString(raw)
	var b strings.Builder
	for i := 0; i < len(enc); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(enc[i:min(i+4, len(enc))])
	}
	return key, b.String()
}

// ParseRecoveryKey decodes a recovery key typed by a user. Separators and
// whitespace are ignored, and the letters I, L and O are read as 1, 1 and 0.
func ParseRecoveryKey(text string) ([]byte, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(text) {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if !strings.ContainsRune(crockford, r) {
			return nil, ErrRecoveryKeyFormat
		}
		b.WriteRune(r)
	}
	if b.Len() != recoveryEncoding.EncodedLen(recoveryKeySize+recoveryChecksumSize) {
		return nil, ErrRecoveryKeyFormat
	}
	raw, err := recoveryEncoding.DecodeString(b.String())
	if err != nil || len(raw) != recoveryKeySize+recoveryChecksumSize {
		return nil, ErrRecoveryKeyFormat
	}
	key, sum := raw[:recoveryKeySize], raw[recoveryKeySize:]
	if !bytes.Equal(sum, recoveryChecksum(key)) {
		return nil, ErrRecoveryKeyFormat
	}
	return key, nil
}
