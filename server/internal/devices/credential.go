package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// TokenPrefix marks a photobackup device token wherever one turns up — a log
// line, an env file, a pasted shell command. Recognising a leaked credential on
// sight is worth four characters.
const TokenPrefix = "pbk_"

const (
	// tokenBytes is the size of the secret inside a token. 256 bits of
	// randomness, which is why a digest is the right way to store it: there is
	// no dictionary to attack and nothing for a password KDF to slow down.
	tokenBytes = 32
	// codeBytes is 40 bits, which is exactly eight characters of base32.
	codeBytes = 5
	codeChars = 8
)

// codeAlphabet is Crockford base32: no I, L, O or U, so nothing in a printed
// code can be confused with a digit and nothing spells anything.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrMalformedCode means the input could not be a pairing code at all, as
// opposed to being one this server does not hold.
var ErrMalformedCode = errors.New("devices: not a pairing code")

// newToken mints a device token and the digest to store for it.
func newToken() (token string, digest []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, digestOf(token), nil
}

// newCode mints a pairing code in normalized form.
func newCode() (string, error) {
	buf := make([]byte, codeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}

	// Five bytes to eight characters, five bits at a time, most significant
	// first. Not encoding/base32: that alphabet includes I, L and O.
	var acc uint64
	for _, b := range buf {
		acc = acc<<8 | uint64(b)
	}
	out := make([]byte, codeChars)
	for i := codeChars - 1; i >= 0; i-- {
		out[i] = codeAlphabet[acc&0x1f]
		acc >>= 5
	}
	return string(out), nil
}

// NormalizeCode reduces however a human typed a code to the one form the server
// compares.
//
// Everything that is not a code character is dropped, so the grouping dash from
// Format costs nothing and neither does a stray space. The three characters the
// alphabet leaves out are folded onto the digits they look like, because
// somebody reading "0" off a terminal and typing "O" has not made a mistake
// worth failing a pairing over.
func NormalizeCode(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	if b.Len() != codeChars {
		return "", fmt.Errorf("%w: expected %d characters, got %d", ErrMalformedCode, codeChars, b.Len())
	}
	return b.String(), nil
}

// FormatCode groups a code for reading aloud and typing in.
func FormatCode(code string) string {
	if len(code) != codeChars {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// digestOf is how both credentials are stored. sha256 rather than a password
// hash on purpose: these are 40- and 256-bit random values, so there is no
// guessing attack for a slow KDF to frustrate — and every chunk of a multi-
// gigabyte video authenticates, which is several hundred verifications per
// upload that argon2 would make expensive for nothing.
func digestOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
