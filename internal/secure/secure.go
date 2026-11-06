// Package secure holds password hashing, random tokens, ids and encryption at rest.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/argon2"
)

var idEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// ID returns a short random id such as "p_x7k2m9qd3f8h".
func ID(prefix string) string {
	b := make([]byte, 10)
	mustRand(b)
	return prefix + "_" + idEnc.EncodeToString(b)
}

// Token returns a URL-safe random token with 256 bits of entropy.
func Token() string {
	b := make([]byte, 32)
	mustRand(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash is a fast one-way hash for high-entropy tokens. Passwords use HashPassword instead.
func Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// Code returns a uniformly random 6-digit verification code.
func Code() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is unrecoverable
	}
}

/* ---------- passwords (argon2id) ---------- */

const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns an encoded argon2id hash with its parameters and salt.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	mustRand(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", argonTime, argonMemory, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// CheckPassword compares a password against an encoded hash in constant time.
func CheckPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var t, m uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &t, &m, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyPasswordCheck spends the same time as a real check, so a login for an unknown
// account can't be told apart from a wrong password by timing.
func DummyPasswordCheck(password string) {
	argon2.IDKey([]byte(password), []byte("wearwhat-dummy-salt"), argonTime, argonMemory, argonThreads, argonKeyLen)
}

/* ---------- encryption at rest (AES-256-GCM) ---------- */

// Encrypt seals plaintext with the 32-byte key. Empty input stays empty.
func Encrypt(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	sealed, err := EncryptBytes(key, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
func Decrypt(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	plain, err := DecryptBytes(key, raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// EncryptBytes seals data with the 32-byte key. The result carries its own nonce.
func EncryptBytes(key, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	mustRand(nonce)
	return gcm.Seal(nonce, nonce, data, nil), nil
}

// DecryptBytes reverses EncryptBytes, and fails if the key is wrong or the data was altered.
func DecryptBytes(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed value too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
