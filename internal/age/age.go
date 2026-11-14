package age

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

const (
	ageHeaderLine   = "age-encryption.org/v1"
	x25519Info      = "age-encryption.org/v1/X25519"
	headerInfo      = "header"
	payloadInfo     = "payload"
	fileKeySize     = 16
	streamNonceSize = 16
	chunkPlainSize  = 64 * 1024
	chunkCipherSize = chunkPlainSize + polyTagSize
	maxHeaderBytes  = 64 * 1024
)

var strictB64 = base64.RawStdEncoding.Strict()

// Recipient is an age X25519 public key ("age1...").
type Recipient struct {
	pub *ecdh.PublicKey
}

// String formats the recipient in canonical lowercase Bech32 ("age1...").
func (r Recipient) String() string {
	if r.pub == nil {
		return ""
	}
	data5, _ := convertBits(r.pub.Bytes(), 8, 5, true)
	return bech32Encode("age", data5, false)
}

// ParseRecipient parses a canonical lowercase "age1..." X25519 recipient.
func ParseRecipient(s string) (Recipient, error) {
	s = strings.TrimSpace(s)
	hrp, data5, isUpper, err := bech32Decode(s)
	if err != nil || isUpper || hrp != "age" {
		return Recipient{}, pferr.Parsef("the age recipient key is malformed")
	}
	raw, err := convertBits(data5, 5, 8, false)
	if err != nil || len(raw) != 32 {
		return Recipient{}, pferr.Parsef("the age recipient key is malformed")
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return Recipient{}, pferr.Parsef("the age recipient key is malformed")
	}
	return Recipient{pub: pub}, nil
}

// ParseRecipientFile parses the first non-empty, non-comment recipient line
// from a configuration file.
func ParseRecipientFile(content []byte) (Recipient, error) {
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return ParseRecipient(line)
	}
	return Recipient{}, pferr.Parsef("no age recipient key was found")
}

// Identity is an age X25519 private key ("AGE-SECRET-KEY-1...").
type Identity struct {
	priv *ecdh.PrivateKey
}

// GenerateIdentity creates a new random X25519 age identity.
func GenerateIdentity() (Identity, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, pferr.Parsef("random key generation failed")
	}
	return Identity{priv: priv}, nil
}

// Recipient returns the public Recipient corresponding to this Identity.
func (id Identity) Recipient() Recipient {
	return Recipient{pub: id.priv.PublicKey()}
}

// String formats the identity in canonical uppercase Bech32 ("AGE-SECRET-KEY-1...").
func (id Identity) String() string {
	if id.priv == nil {
		return ""
	}
	data5, _ := convertBits(id.priv.Bytes(), 8, 5, true)
	return bech32Encode("age-secret-key-", data5, true)
}

// ParseIdentity parses an "AGE-SECRET-KEY-1..." X25519 secret key.
func ParseIdentity(s string) (Identity, error) {
	s = strings.TrimSpace(s)
	hrp, data5, isUpper, err := bech32Decode(s)
	if err != nil || !isUpper || hrp != "age-secret-key-" {
		return Identity{}, pferr.Parsef("the age secret key is malformed")
	}
	raw, err := convertBits(data5, 5, 8, false)
	if err != nil || len(raw) != 32 {
		return Identity{}, pferr.Parsef("the age secret key is malformed")
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return Identity{}, pferr.Parsef("the age secret key is malformed")
	}
	return Identity{priv: priv}, nil
}

// ParseIdentityFile parses the first non-empty, non-comment secret key line
// from an age key file (matching the output format of age-keygen).
func ParseIdentityFile(content []byte) (Identity, error) {
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return ParseIdentity(line)
	}
	return Identity{}, pferr.Parsef("no age secret key was found")
}

// Encrypt returns a streaming WriteCloser that encrypts plaintext written to
// it in age-encryption.org/v1 format to dst for recipient r. Callers must
// check the error returned by Close to ensure the final authenticated chunk is
// flushed.
func Encrypt(dst io.Writer, r Recipient) (io.WriteCloser, error) {
	if r.pub == nil {
		return nil, pferr.Parsef("the age recipient key is missing")
	}
	var fileKey [fileKeySize]byte
	if _, err := io.ReadFull(rand.Reader, fileKey[:]); err != nil {
		return nil, pferr.Parsef("random key generation failed")
	}
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, pferr.Parsef("random key generation failed")
	}
	var streamNonce [streamNonceSize]byte
	if _, err := io.ReadFull(rand.Reader, streamNonce[:]); err != nil {
		return nil, pferr.Parsef("random key generation failed")
	}
	return encryptDeterministic(dst, r, fileKey, ephPriv, streamNonce)
}

func encryptDeterministic(dst io.Writer, r Recipient, fileKey [fileKeySize]byte, ephPriv *ecdh.PrivateKey, streamNonce [streamNonceSize]byte) (io.WriteCloser, error) {
	shared, err := ephPriv.ECDH(r.pub)
	if err != nil || isAllZero(shared) {
		return nil, pferr.Parsef("the age recipient key was rejected")
	}
	ephShare := ephPriv.PublicKey().Bytes()
	salt := make([]byte, 64)
	copy(salt[:32], ephShare)
	copy(salt[32:], r.pub.Bytes())

	wrapKeyBytes, err := hkdf.Key(sha256.New, shared, salt, x25519Info, chachaKeySize)
	if err != nil {
		return nil, pferr.Parsef("key derivation failed")
	}
	var wrapKey [chachaKeySize]byte
	copy(wrapKey[:], wrapKeyBytes)

	wrappedFileKey := aeadSeal(wrapKey, [chachaNonceSize]byte{}, fileKey[:], nil)

	var hdr strings.Builder
	hdr.WriteString(ageHeaderLine + "\n")
	hdr.WriteString("-> X25519 " + strictB64.EncodeToString(ephShare) + "\n")
	hdr.WriteString(strictB64.EncodeToString(wrappedFileKey) + "\n")
	hdr.WriteString("---")
	macInput := hdr.String()

	macKey, err := hkdf.Key(sha256.New, fileKey[:], nil, headerInfo, 32)
	if err != nil {
		return nil, pferr.Parsef("key derivation failed")
	}
	hm := hmac.New(sha256.New, macKey)
	hm.Write([]byte(macInput))
	fullHeader := macInput + " " + strictB64.EncodeToString(hm.Sum(nil)) + "\n"

	if _, err := io.WriteString(dst, fullHeader); err != nil {
		return nil, err
	}
	if _, err := dst.Write(streamNonce[:]); err != nil {
		return nil, err
	}

	payloadKeyBytes, err := hkdf.Key(sha256.New, fileKey[:], streamNonce[:], payloadInfo, chachaKeySize)
	if err != nil {
		return nil, pferr.Parsef("key derivation failed")
	}
	var payloadKey [chachaKeySize]byte
	copy(payloadKey[:], payloadKeyBytes)

	return &streamWriter{
		dst: dst,
		key: payloadKey,
	}, nil
}

type streamWriter struct {
	dst     io.Writer
	key     [chachaKeySize]byte
	buf     [chunkPlainSize]byte
	n       int
	counter uint64
	closed  bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, pferr.Parsef("write to closed age stream")
	}
	written := 0
	for len(p) > 0 {
		if w.n == chunkPlainSize {
			if err := w.flushChunk(false); err != nil {
				return written, err
			}
		}
		c := copy(w.buf[w.n:], p)
		w.n += c
		p = p[c:]
		written += c
	}
	return written, nil
}

func (w *streamWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.flushChunk(true)
	w.key = [chachaKeySize]byte{}
	return err
}

func (w *streamWriter) flushChunk(last bool) error {
	nonce := streamChunkNonce(w.counter, last)
	sealed := aeadSeal(w.key, nonce, w.buf[:w.n], nil)
	w.n = 0
	w.counter++
	_, err := w.dst.Write(sealed)
	return err
}

func streamChunkNonce(counter uint64, last bool) [chachaNonceSize]byte {
	var nonce [chachaNonceSize]byte
	binary.BigEndian.PutUint64(nonce[3:11], counter)
	if last {
		nonce[11] = 0x01
	}
	return nonce
}

type stanza struct {
	kind string
	args []string
	body []byte
}

// Decrypt verifies the age-encryption.org/v1 header from src using id and
// returns a streaming Reader yielding authenticated plaintext.
func Decrypt(src io.Reader, id Identity) (io.Reader, error) {
	if id.priv == nil {
		return nil, pferr.Parsef("the age secret key is missing")
	}
	br := bufio.NewReaderSize(src, chunkCipherSize+64)
	stanzas, macInput, wantMAC, err := readAgeHeader(br)
	if err != nil {
		return nil, err
	}

	var fileKey [fileKeySize]byte
	foundKey := false
	myPubBytes := id.priv.PublicKey().Bytes()
	for _, st := range stanzas {
		if st.kind != "X25519" {
			continue
		}
		if len(st.args) != 1 || len(st.body) != fileKeySize+polyTagSize {
			return nil, pferr.Parsef("an X25519 stanza in the archive header is malformed")
		}
		ephBytes, err := strictB64.DecodeString(st.args[0])
		if err != nil || len(ephBytes) != 32 {
			return nil, pferr.Parsef("an X25519 ephemeral share in the archive header is malformed")
		}
		ephPub, err := ecdh.X25519().NewPublicKey(ephBytes)
		if err != nil {
			return nil, pferr.Parsef("an X25519 ephemeral share in the archive header is malformed")
		}
		shared, err := id.priv.ECDH(ephPub)
		if err != nil || isAllZero(shared) {
			return nil, pferr.Parsef("an X25519 ephemeral share in the archive header was rejected")
		}
		salt := make([]byte, 64)
		copy(salt[:32], ephBytes)
		copy(salt[32:], myPubBytes)
		wrapKeyBytes, err := hkdf.Key(sha256.New, shared, salt, x25519Info, chachaKeySize)
		if err != nil {
			return nil, pferr.Parsef("key derivation failed")
		}
		var wrapKey [chachaKeySize]byte
		copy(wrapKey[:], wrapKeyBytes)
		unwrapped, err := aeadOpen(nil, wrapKey, [chachaNonceSize]byte{}, st.body, nil)
		if err != nil {
			continue
		}
		copy(fileKey[:], unwrapped)
		foundKey = true
		break
	}
	if !foundKey {
		return nil, pferr.Parsef("no recipient stanza matched the provided age secret key")
	}

	macKey, err := hkdf.Key(sha256.New, fileKey[:], nil, headerInfo, 32)
	if err != nil {
		return nil, pferr.Parsef("key derivation failed")
	}
	hm := hmac.New(sha256.New, macKey)
	hm.Write(macInput)
	if !hmac.Equal(hm.Sum(nil), wantMAC) {
		return nil, pferr.Parsef("the encrypted archive header failed authentication")
	}

	var streamNonce [streamNonceSize]byte
	if _, err := io.ReadFull(br, streamNonce[:]); err != nil {
		return nil, pferr.Parsef("the encrypted archive ended before its payload nonce")
	}
	payloadKeyBytes, err := hkdf.Key(sha256.New, fileKey[:], streamNonce[:], payloadInfo, chachaKeySize)
	if err != nil {
		return nil, pferr.Parsef("key derivation failed")
	}
	var payloadKey [chachaKeySize]byte
	copy(payloadKey[:], payloadKeyBytes)

	return &streamReader{
		br:  br,
		key: payloadKey,
	}, nil
}

func readAgeHeader(br *bufio.Reader) ([]stanza, []byte, []byte, error) {
	var rawHeader bytes.Buffer
	readLine := func() (string, error) {
		if rawHeader.Len() > maxHeaderBytes {
			return "", pferr.Needsf("the encrypted archive header is larger than Plainforge handles")
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return "", pferr.Parsef("the encrypted archive header ended unexpectedly")
		}
		if rawHeader.Len()+len(line) > maxHeaderBytes {
			return "", pferr.Needsf("the encrypted archive header is larger than Plainforge handles")
		}
		if strings.ContainsRune(line, '\r') {
			return "", pferr.Parsef("the encrypted archive header used non-LF line endings")
		}
		rawHeader.WriteString(line)
		return strings.TrimSuffix(line, "\n"), nil
	}

	first, err := readLine()
	if err != nil {
		return nil, nil, nil, err
	}
	if first != ageHeaderLine {
		return nil, nil, nil, pferr.Parsef("the file did not start with an age v1 header")
	}

	var stanzas []stanza
	for {
		line, err := readLine()
		if err != nil {
			return nil, nil, nil, err
		}
		if afterDash, ok := strings.CutPrefix(line, "---"); ok {
			if len(stanzas) == 0 {
				return nil, nil, nil, pferr.Parsef("the encrypted archive header had no recipient stanzas")
			}
			macB64, ok := strings.CutPrefix(afterDash, " ")
			if !ok || len(macB64) != 43 {
				return nil, nil, nil, pferr.Parsef("the encrypted archive header MAC is malformed")
			}
			wantMAC, err := strictB64.DecodeString(macB64)
			if err != nil || len(wantMAC) != 32 {
				return nil, nil, nil, pferr.Parsef("the encrypted archive header MAC is malformed")
			}
			// macInput is everything up to and including "---", excluding " <mac>\n".
			all := rawHeader.Bytes()
			cutoff := len(all) - len(afterDash) - 1
			macInput := make([]byte, cutoff)
			copy(macInput, all[:cutoff])
			return stanzas, macInput, wantMAC, nil
		}

		argLine, ok := strings.CutPrefix(line, "-> ")
		if !ok {
			return nil, nil, nil, pferr.Parsef("the encrypted archive header had an unexpected line")
		}
		parts := strings.Split(argLine, " ")
		if len(parts) == 0 || parts[0] == "" {
			return nil, nil, nil, pferr.Parsef("the encrypted archive header had an empty stanza type")
		}
		for _, p := range parts {
			if p == "" || !isPrintableASCII(p) {
				return nil, nil, nil, pferr.Parsef("the encrypted archive header had malformed stanza arguments")
			}
		}

		var bodyB64 strings.Builder
		for {
			bline, err := readLine()
			if err != nil {
				return nil, nil, nil, err
			}
			if len(bline) > 64 {
				return nil, nil, nil, pferr.Parsef("an archive stanza body line exceeded 64 characters")
			}
			bodyB64.WriteString(bline)
			if len(bline) < 64 {
				break
			}
		}
		bodyBytes, err := strictB64.DecodeString(bodyB64.String())
		if err != nil {
			return nil, nil, nil, pferr.Parsef("an archive stanza body had malformed base64")
		}
		stanzas = append(stanzas, stanza{
			kind: parts[0],
			args: parts[1:],
			body: bodyBytes,
		})
	}
}

type streamReader struct {
	br       *bufio.Reader
	key      [chachaKeySize]byte
	cipher   [chunkCipherSize]byte
	plainBuf []byte
	plain scratchBuf
	counter  uint64
	done     bool
	err      error
}

type scratchBuf [chunkPlainSize]byte

func (r *streamReader) Read(p []byte) (int, error) {
	if len(r.plainBuf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.nextChunk(); err != nil {
			r.err = err
			return 0, err
		}
		if len(r.plainBuf) == 0 && r.done {
			return 0, io.EOF
		}
	}
	n := copy(p, r.plainBuf)
	r.plainBuf = r.plainBuf[n:]
	return n, nil
}

func (r *streamReader) nextChunk() error {
	n, err := io.ReadFull(r.br, r.cipher[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if n == 0 {
		return pferr.Parsef("the encrypted archive ended before its final chunk")
	}
	if n < polyTagSize {
		return pferr.Parsef("the encrypted archive has a truncated chunk")
	}

	_, peekErr := r.br.Peek(1)
	isAtEOF := errors.Is(peekErr, io.EOF)
	if peekErr != nil && !isAtEOF {
		return peekErr
	}
	if !isAtEOF && n < chunkCipherSize {
		return pferr.Parsef("the encrypted archive has trailing bytes after its final chunk")
	}

	last := isAtEOF
	if last && n == polyTagSize && r.counter > 0 {
		return pferr.Parsef("the encrypted archive had an empty final chunk")
	}

	nonce := streamChunkNonce(r.counter, last)
	opened, openErr := aeadOpen(r.plain[:0], r.key, nonce, r.cipher[:n], nil)
	if openErr != nil {
		return pferr.Parsef("an encrypted archive chunk failed authentication")
	}
	r.plainBuf = opened
	r.counter++
	if last {
		r.done = true
		r.key = [chachaKeySize]byte{}
	}
	return nil
}

func isAllZero(b []byte) bool {
	return subtle.ConstantTimeCompare(b, make([]byte, len(b))) == 1
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 33 || s[i] > 126 {
			return false
		}
	}
	return true
}
