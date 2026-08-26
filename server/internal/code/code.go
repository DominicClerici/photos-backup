// Package code is the short credential somebody reads off a terminal and types
// somewhere else.
//
// It was invented for device pairing and is now used by two callers — pairing a
// phone and enrolling a passkey — which is why it lives here rather than in
// either of them. The format is the whole of the package: eight characters of
// Crockford base32, forgiving about how they were typed and unambiguous about
// what they were.
package code

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	// Bytes is 40 bits, which is exactly eight characters of base32.
	Bytes = 5
	// Chars is the length of a code in its normalized form.
	Chars = 8
)

// Alphabet is Crockford base32: no I, L, O or U, so nothing in a printed code
// can be confused with a digit and nothing spells anything.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrMalformed means the input could not be a code at all, as opposed to being
// one no server holds.
var ErrMalformed = errors.New("code: not a valid code")

// New mints a code in normalized form.
func New() (string, error) {
	buf := make([]byte, Bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate code: %w", err)
	}

	// Five bytes to eight characters, five bits at a time, most significant
	// first. Not encoding/base32: that alphabet includes I, L and O.
	var acc uint64
	for _, b := range buf {
		acc = acc<<8 | uint64(b)
	}
	out := make([]byte, Chars)
	for i := Chars - 1; i >= 0; i-- {
		out[i] = Alphabet[acc&0x1f]
		acc >>= 5
	}
	return string(out), nil
}

// Normalize reduces however a human typed a code to the one form a server
// compares.
//
// Everything that is not a code character is dropped, so the grouping dash from
// Format costs nothing and neither does a stray space. The three characters the
// alphabet leaves out are folded onto the digits they look like, because
// somebody reading "0" off a terminal and typing "O" has not made a mistake
// worth failing a pairing over.
func Normalize(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if strings.ContainsRune(Alphabet, r) {
			b.WriteRune(r)
		}
	}
	if b.Len() != Chars {
		return "", fmt.Errorf("%w: expected %d characters, got %d", ErrMalformed, Chars, b.Len())
	}
	return b.String(), nil
}

// Format groups a code for reading aloud and typing in.
func Format(c string) string {
	if len(c) != Chars {
		return c
	}
	return c[:4] + "-" + c[4:]
}
