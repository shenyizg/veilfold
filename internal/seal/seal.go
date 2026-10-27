// Package seal contains Veilfold's cryptographic building blocks: password
// based key derivation, small-message authenticated encryption used for key
// wrapping and the vault index, a chunked streaming AEAD for folder contents,
// and recovery keys.
//
// Everything here is built from well-reviewed primitives:
//
//   - Argon2id (RFC 9106) for password based key derivation,
//   - HKDF-SHA256 (RFC 5869) for deriving purpose-bound sub keys,
//   - XChaCha20-Poly1305 for small messages with random nonces,
//   - ChaCha20-Poly1305 (RFC 8439) in the STREAM construction for large data.
package seal

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the size in bytes of every symmetric key used by Veilfold.
const KeySize = 32

// ErrAuth is returned when a ciphertext fails authentication. This happens
// when the key is wrong or the data was corrupted or tampered with.
var ErrAuth = errors.New("authentication failed: wrong key, or the data is corrupted")

// KDFParams is an Argon2id parameter set. It is stored next to every
// password-protected key so that parameters can be raised in the future
// without breaking existing vaults.
type KDFParams struct {
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
}

// DefaultKDF is used for new vaults: 64 MiB of memory, 3 passes, 4 lanes.
var DefaultKDF = KDFParams{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 4}

// Validate rejects parameter sets that are too weak to be meaningful or so
// large that a tampered vault header could exhaust the machine's memory.
func (p KDFParams) Validate() error {
	switch {
	case p.Parallelism < 1 || p.Parallelism > 64:
		return fmt.Errorf("argon2id: parallelism %d out of range", p.Parallelism)
	case p.Iterations < 1 || p.Iterations > 64:
		return fmt.Errorf("argon2id: iterations %d out of range", p.Iterations)
	case p.MemoryKiB < 8*uint32(p.Parallelism) || p.MemoryKiB > 4*1024*1024:
		return fmt.Errorf("argon2id: memory %d KiB out of range", p.MemoryKiB)
	}
	return nil
}

// DeriveKey stretches a password into a key with Argon2id.
func DeriveKey(password, salt []byte, p KDFParams) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(salt) < 16 {
		return nil, errors.New("argon2id: salt too short")
	}
	return argon2.IDKey(password, salt, p.Iterations, p.MemoryKiB, p.Parallelism, KeySize), nil
}

// SubKey derives an independent key for a specific purpose from a master key.
func SubKey(master, salt []byte, info string) []byte {
	k, err := hkdf.Key(sha256.New, master, salt, info, KeySize)
	if err != nil {
		// Only possible for absurd output lengths.
		panic(err)
	}
	return k
}

// Random returns n bytes from the operating system's CSPRNG.
func Random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

// Wipe overwrites b with zeros. Go cannot guarantee that no other copy of a
// secret exists in memory, but wiping keys we own narrows the window.
func Wipe(b []byte) {
	clear(b)
}

// Seal encrypts and authenticates plaintext with XChaCha20-Poly1305 under a
// fresh random nonce. ad is authenticated but not encrypted. The result is
// nonce || ciphertext || tag.
func Seal(key, plaintext, ad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := Random(aead.NonceSize())
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, ad), nil
}

// Open reverses Seal. It returns ErrAuth if the key or ad is wrong or the
// message was modified.
func Open(key, sealed, ad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrAuth
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, ad)
	if err != nil {
		return nil, ErrAuth
	}
	return pt, nil
}
