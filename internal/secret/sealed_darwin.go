//go:build darwin

package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// A secret is two keychain items. The item under the secret's name holds a
// 32-byte key, written through `security -i` on stdin, where a line is
// capped at 4096 bytes and a key never comes near that. The item under the
// sealed account holds the value encrypted under that key, written through
// security's argv, which has no such cap.
//
// A command line is readable by other processes on the host, so the sealed
// write puts only ciphertext and the secret's name there. The value itself
// is never on any command line, at any size, and the key reaches security
// only down a pipe.
//
// What this does not change: a process that can read the keychain as you
// could read every secret before and can read both items now. That boundary
// is the one docs/security.md describes, and it is the same.

// keyPrefix marks a keychain item as holding a key rather than a value.
//
// It sits outside the base64, not inside it, for the brig that came before
// this change. That brig reads every item as base64 of the value, and a key
// item that decoded cleanly would reach a guest as a 32-byte credential. A
// colon is not a base64 character, so the older read fails instead, with the
// refusal it already has for an item brig did not write.
//
// An item written before this change holds the value itself, base64 and no
// prefix, and Read returns it as it always did.
const keyPrefix = "brigsealed1:"

// keyLen is AES-256.
const keyLen = 32

// sealedMagic opens the sealed blob, so an item that is not one is named as
// such rather than failing inside the cipher.
const sealedMagic = "BRIGSEAL"

// nonceLen is AES-GCM's standard nonce. A fresh random nonce per write is
// safe for far more writes than any secret will ever see.
const nonceLen = 12

// sealedSuffix ends the sealed item's account. The dot is outside the name
// grammar, so no secret can be named like one and List, which skips names
// outside the grammar, never shows one.
const sealedSuffix = ".sealed"

// sealedAccount is the keychain account of the sealed item for name.
func sealedAccount(name string) string { return name + sealedSuffix }

// newKey draws a fresh key from the system's random source.
func newKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("cannot draw a key: %w", err)
	}
	return key, nil
}

// keyItem is the line the key item holds.
func keyItem(key []byte) string {
	return keyPrefix + base64.StdEncoding.EncodeToString(key)
}

// keyFromItem reads the key out of an item line, or reports that the line
// is a pre-sealing value. A marked line that does not hold a key of the
// right size was written by something other than brig.
func keyFromItem(line string) (key []byte, sealed bool, err error) {
	rest, ok := strings.CutPrefix(line, keyPrefix)
	if !ok {
		return nil, false, nil
	}
	key, err = base64.StdEncoding.DecodeString(rest)
	if err != nil || len(key) != keyLen {
		return nil, true, errors.New("the keychain item is marked as a key and does not hold one, so brig did not write it")
	}
	return key, true, nil
}

// seal encrypts value for name under key: magic, nonce, then the ciphertext
// with its tag. The name is the additional data, so a sealed item cannot be
// moved under another secret's name and still open.
func seal(name string, key, value []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("cannot draw a nonce: %w", err)
	}
	out := make([]byte, 0, len(sealedMagic)+nonceLen+len(value)+gcm.Overhead())
	out = append(out, sealedMagic...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, value, []byte(name)), nil
}

// errNotSealed means the blob is not in the sealed format at all.
var errNotSealed = errors.New("not a brig sealed value")

// unseal reverses seal. A blob that does not open with this key was sealed
// under another key, or changed since, and the cipher's own error says
// neither; the caller names what happened.
func unseal(name string, key, blob []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(blob, []byte(sealedMagic))
	if !ok || len(rest) < nonceLen {
		return nil, errNotSealed
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, rest[:nonceLen], rest[nonceLen:], []byte(name))
}
