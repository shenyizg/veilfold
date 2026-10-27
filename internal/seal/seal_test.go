package seal

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

var testKDF = KDFParams{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func TestDeriveKeyDeterministic(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, 16)
	a, err := DeriveKey([]byte("correct horse"), salt, testKDF)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DeriveKey([]byte("correct horse"), salt, testKDF)
	c, _ := DeriveKey([]byte("correct horsf"), salt, testKDF)
	if !bytes.Equal(a, b) || bytes.Equal(a, c) || len(a) != KeySize {
		t.Fatal("argon2id derivation is not deterministic or not password dependent")
	}
}

func TestKDFValidate(t *testing.T) {
	bad := []KDFParams{
		{MemoryKiB: 64, Iterations: 0, Parallelism: 1},
		{MemoryKiB: 64, Iterations: 1, Parallelism: 0},
		{MemoryKiB: 4, Iterations: 1, Parallelism: 1},
		{MemoryKiB: 1 << 30, Iterations: 1, Parallelism: 1},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("params %+v accepted", p)
		}
	}
	if err := DefaultKDF.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSealOpen(t *testing.T) {
	key := Random(KeySize)
	msg := []byte("the vault index")
	ct, err := Seal(key, msg, []byte("ad"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(key, ct, []byte("ad"))
	if err != nil || !bytes.Equal(pt, msg) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := Open(key, ct, []byte("other ad")); !errors.Is(err, ErrAuth) {
		t.Fatal("associated data not authenticated")
	}
	if _, err := Open(Random(KeySize), ct, []byte("ad")); !errors.Is(err, ErrAuth) {
		t.Fatal("wrong key accepted")
	}
	ct[len(ct)-1] ^= 1
	if _, err := Open(key, ct, []byte("ad")); !errors.Is(err, ErrAuth) {
		t.Fatal("tampering not detected")
	}
	if _, err := Open(key, ct[:10], nil); !errors.Is(err, ErrAuth) {
		t.Fatal("short input accepted")
	}
}

func encryptStream(t *testing.T, key, ad, data []byte, writeSize int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, key, ad)
	if err != nil {
		t.Fatal(err)
	}
	for p := data; len(p) > 0; {
		n := min(writeSize, len(p))
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decryptStream(key, ad, ct []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(ct), key, ad)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestStreamRoundTrip(t *testing.T) {
	key := Random(KeySize)
	ad := []byte("header")
	sizes := []int{0, 1, 100, ChunkSize - 1, ChunkSize, ChunkSize + 1, 2 * ChunkSize, 3*ChunkSize + 17}
	for _, size := range sizes {
		data := Random(size)
		for _, ws := range []int{1 << 20, 4096, 7} {
			if size > 2*ChunkSize && ws == 7 {
				continue // too slow, adds nothing
			}
			ct := encryptStream(t, key, ad, data, ws)
			chunks := max(1, (size+ChunkSize-1)/ChunkSize)
			if want := size + chunks*tagSize; len(ct) != want {
				t.Fatalf("size %d: ciphertext is %d bytes, want %d", size, len(ct), want)
			}
			got, err := decryptStream(key, ad, ct)
			if err != nil {
				t.Fatalf("size %d write %d: %v", size, ws, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("size %d write %d: plaintext mismatch", size, ws)
			}
		}
	}
}

func TestStreamDetectsManipulation(t *testing.T) {
	key := Random(KeySize)
	ad := []byte("header")
	data := Random(3*ChunkSize + 1000)
	ct := encryptStream(t, key, ad, data, 1<<20)

	cases := map[string][]byte{
		"truncated at chunk boundary": ct[:2*sealedChunk],
		"truncated mid chunk":         ct[:2*sealedChunk+100],
		"only tag bytes":              ct[:10],
		"empty":                       {},
		"appended garbage":            append(append([]byte(nil), ct...), 1, 2, 3),
		"chunks swapped": func() []byte {
			c := append([]byte(nil), ct...)
			a := append([]byte(nil), c[:sealedChunk]...)
			copy(c[:sealedChunk], c[sealedChunk:2*sealedChunk])
			copy(c[sealedChunk:2*sealedChunk], a)
			return c
		}(),
		"bit flip": func() []byte {
			c := append([]byte(nil), ct...)
			c[ChunkSize+5] ^= 0x80
			return c
		}(),
	}
	for name, c := range cases {
		if _, err := decryptStream(key, ad, c); err == nil {
			t.Errorf("%s: manipulation not detected", name)
		}
	}
	if _, err := decryptStream(key, []byte("other"), ct); !errors.Is(err, ErrAuth) {
		t.Error("associated data not authenticated")
	}
	if _, err := decryptStream(Random(KeySize), ad, ct); !errors.Is(err, ErrAuth) {
		t.Error("wrong key accepted")
	}
}

func TestStreamFullFinalChunkTruncation(t *testing.T) {
	// With an exact multiple of ChunkSize the final chunk is full-sized, so
	// dropping it leaves a stream that ends on a non-final full chunk.
	key := Random(KeySize)
	ct := encryptStream(t, key, nil, Random(2*ChunkSize), 1<<20)
	if _, err := decryptStream(key, nil, ct[:sealedChunk]); !errors.Is(err, ErrAuth) {
		t.Fatalf("dropping the final chunk not detected: %v", err)
	}
}

func TestStreamEmptyFinalChunkRejected(t *testing.T) {
	// Forge "chunk 0 (not last) + empty last chunk": a valid writer never
	// produces it and a reader must not accept it.
	key := Random(KeySize)
	w, _ := NewWriter(io.Discard, key, nil)
	c0 := w.aead.Seal(nil, streamNonce(0, false), Random(ChunkSize), nil)
	c1 := w.aead.Seal(nil, streamNonce(1, true), nil, nil)
	if _, err := decryptStream(key, nil, append(c0, c1...)); err == nil {
		t.Fatal("empty final chunk after data accepted")
	}
}

func TestRecoveryKey(t *testing.T) {
	key, text := NewRecoveryKey()
	if len(text) != 39 || strings.Count(text, "-") != 7 {
		t.Fatalf("unexpected format %q", text)
	}
	got, err := ParseRecoveryKey(text)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("round trip failed: %v", err)
	}
	// Lower case, no separators, spaces and confusable letters are accepted.
	loose := strings.ToLower(strings.ReplaceAll(text, "-", " "))
	loose = strings.ReplaceAll(loose, "0", "o")
	loose = strings.ReplaceAll(loose, "1", "l")
	if got, err := ParseRecoveryKey(loose); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("lenient parsing failed: %v", err)
	}
	// A single typo is caught by the checksum.
	typo := []byte(text)
	if typo[0] == 'A' {
		typo[0] = 'B'
	} else {
		typo[0] = 'A'
	}
	if _, err := ParseRecoveryKey(string(typo)); !errors.Is(err, ErrRecoveryKeyFormat) {
		t.Fatal("typo not detected")
	}
	for _, bad := range []string{"", "ABCD", text + "A", strings.Replace(text, text[:1], "U", 1)} {
		if _, err := ParseRecoveryKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
