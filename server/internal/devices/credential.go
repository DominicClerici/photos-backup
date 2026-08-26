package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/dominicclerici/photos-backup/server/internal/code"
)

// TokenPrefix marks a photobackup device token wherever one turns up — a log
// line, an env file, a pasted shell command. Recognising a leaked credential on
// sight is worth four characters.
const TokenPrefix = "pbk_"

// tokenBytes is the size of the secret inside a token. 256 bits of randomness,
// which is why a digest is the right way to store it: there is no dictionary to
// attack and nothing for a password KDF to slow down.
const tokenBytes = 32

// codeChars is the pairing code's length, re-exported into this package's own
// namespace so the tests that assert the format read against the credential
// they are about rather than against the package it is implemented in.
const codeChars = code.Chars

// ErrMalformedCode means the input could not be a pairing code at all, as
// opposed to being one this server does not hold.
//
// The same value the code package returns, so `errors.Is(err, ErrMalformedCode)`
// holds for errors raised in either place.
var ErrMalformedCode = code.ErrMalformed

// newToken mints a device token and the digest to store for it.
func newToken() (token string, digest []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, digestOf(token), nil
}

// The pairing code's format lives in internal/code, which the passkey
// enrollment flow also uses. These three stay here because the pairing API is
// what the CLI and the phone are written against, and moving them would be a
// rename for its own sake.

// newCode mints a pairing code in normalized form.
func newCode() (string, error) { return code.New() }

// NormalizeCode reduces however a human typed a code to the one form the server
// compares.
func NormalizeCode(raw string) (string, error) { return code.Normalize(raw) }

// FormatCode groups a code for reading aloud and typing in.
func FormatCode(c string) string { return code.Format(c) }

// digestOf is how both credentials are stored. sha256 rather than a password
// hash on purpose: these are 40- and 256-bit random values, so there is no
// guessing attack for a slow KDF to frustrate — and every chunk of a multi-
// gigabyte video authenticates, which is several hundred verifications per
// upload that argon2 would make expensive for nothing.
func digestOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
