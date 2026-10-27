package seal

import (
	"bufio"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// The streaming format follows the STREAM construction (Hoang, Reyhanitabar,
// Rogaway, Vizár: "Online Authenticated-Encryption and its Nonce-Reuse
// Misuse-Resistance", 2015), as also used by age:
//
//   - plaintext is split into chunks of ChunkSize bytes; only the last chunk
//     may be shorter, and it is empty only if the whole plaintext is empty,
//   - each chunk is sealed with ChaCha20-Poly1305 using the nonce
//     counter (11 bytes, big endian) || last-chunk flag (1 byte),
//   - every chunk authenticates the same associated data (the blob header).
//
// Because the key is unique per stream, counter nonces never repeat. Because
// the final chunk is marked, truncating the stream at a chunk boundary,
// reordering, dropping or appending chunks are all detected.

// ChunkSize is the plaintext size of every chunk but the last.
const ChunkSize = 64 * 1024

const (
	tagSize       = chacha20poly1305.Overhead
	sealedChunk   = ChunkSize + tagSize
	lastChunkFlag = 0x01
)

// ErrTruncated is returned when an encrypted stream ends early.
var ErrTruncated = errors.New("encrypted data is truncated")

func streamNonce(counter uint64, last bool) []byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(n[3:11], counter)
	if last {
		n[11] = lastChunkFlag
	}
	return n[:]
}

// Writer encrypts a stream. Close must be called to write the final chunk;
// without it the stream is unreadable (truncated).
type Writer struct {
	aead    cipher.AEAD
	dst     io.Writer
	ad      []byte
	buf     []byte
	out     []byte
	counter uint64
	closed  bool
	err     error
}

// NewWriter returns a Writer that encrypts to dst with a 32-byte key. The key
// must never be used for another stream.
func NewWriter(dst io.Writer, key, ad []byte) (*Writer, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &Writer{
		aead: aead,
		dst:  dst,
		ad:   append([]byte(nil), ad...),
		buf:  make([]byte, 0, ChunkSize),
		out:  make([]byte, 0, sealedChunk),
	}, nil
}

// Write encrypts p. A full chunk is only flushed once more data arrives, so
// that the last chunk of the stream is never empty unless the stream is.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("seal: write to closed stream")
	}
	n := 0
	for len(p) > 0 {
		if len(w.buf) == ChunkSize {
			if err := w.flush(false); err != nil {
				w.err = err
				return n, err
			}
		}
		k := copy(w.buf[len(w.buf):ChunkSize], p)
		w.buf = w.buf[:len(w.buf)+k]
		p = p[k:]
		n += k
	}
	return n, nil
}

// Close writes the final chunk. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.flush(true); err != nil {
		w.err = err
		return err
	}
	return nil
}

func (w *Writer) flush(last bool) error {
	w.out = w.aead.Seal(w.out[:0], streamNonce(w.counter, last), w.buf, w.ad)
	if _, err := w.dst.Write(w.out); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	w.counter++
	return nil
}

// Reader decrypts a stream written by Writer. Data is only returned after
// the chunk containing it has been authenticated. Read returns io.EOF only
// after the authenticated final chunk; an early end of the input yields
// ErrTruncated or ErrAuth.
type Reader struct {
	aead    cipher.AEAD
	src     *bufio.Reader
	ad      []byte
	in      []byte
	out     []byte
	pos     int
	counter uint64
	done    bool
	err     error
}

// NewReader returns a Reader that decrypts src.
func NewReader(src io.Reader, key, ad []byte) (*Reader, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &Reader{
		aead: aead,
		src:  bufio.NewReaderSize(src, sealedChunk+1),
		ad:   append([]byte(nil), ad...),
		in:   make([]byte, sealedChunk),
		out:  make([]byte, 0, ChunkSize),
	}, nil
}

func (r *Reader) Read(p []byte) (int, error) {
	for r.pos == len(r.out) {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.out[r.pos:])
	r.pos += n
	return n, nil
}

func (r *Reader) next() error {
	n, err := io.ReadFull(r.src, r.in)
	last := false
	switch {
	case err == io.EOF:
		return ErrTruncated
	case err == io.ErrUnexpectedEOF:
		last = true // a short chunk can only be the final one
	case err != nil:
		return err
	default:
		// A full chunk is the final one exactly when nothing follows it.
		if _, perr := r.src.Peek(1); perr == io.EOF {
			last = true
		} else if perr != nil {
			return perr
		}
	}
	if n < tagSize {
		return ErrTruncated
	}
	out, err := r.aead.Open(r.out[:0], streamNonce(r.counter, last), r.in[:n], r.ad)
	if err != nil {
		return ErrAuth
	}
	if last && len(out) == 0 && r.counter > 0 {
		// Only an entirely empty stream may end with an empty chunk.
		return ErrAuth
	}
	r.out, r.pos = out, 0
	r.counter++
	r.done = last
	return nil
}
