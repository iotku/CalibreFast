package main

// Secrets CalibreFast must send on again (a user's Hardcover tokens), kept
// encrypted in the app database: AES-256-GCM with a key in its own file in
// -datadir (secretKeyFile, mode 0600), made the first time one is sealed.
// A copy of calibrefast.db alone holds no usable token.
//
// Each secret is bound to what it's for (the "context": the table, the
// column and the user's id), so a sealed value copied into another row or
// to another user doesn't open.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const secretKeyFile = "secrets.key"

// sealPrefix marks a sealed value (and its version).
const sealPrefix = "v1:"

var secretKey struct {
	sync.Mutex
	key  []byte
	path string // where it was read from
}

// secretKeyPath is the key file (a variable for tests).
var secretKeyPath = func() string { return filepath.Join(dataDir, secretKeyFile) }

// loadSecretKey reads the key, making it when create is set and there is
// none.
func loadSecretKey(create bool) ([]byte, error) {
	secretKey.Lock()
	defer secretKey.Unlock()
	path := secretKeyPath()
	if secretKey.key != nil && secretKey.path == path {
		return secretKey.key, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(key) + "\n"
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = f.WriteString(enc)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(path)
				return nil, fmt.Errorf("writing %s: %w", secretKeyFile, err)
			}
			raw = []byte(enc)
		} else if errors.Is(err, os.ErrExist) {
			raw, err = os.ReadFile(path) // made meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("making %s: %w", secretKeyFile, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading %s: %w", secretKeyFile, err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s isn't a key (32 bytes, base64)", secretKeyFile)
	}
	secretKey.key, secretKey.path = key, path
	return key, nil
}

func secretAEAD(create bool) (cipher.AEAD, error) {
	key, err := loadSecretKey(create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealSecret encrypts plain for context.
func sealSecret(context, plain string) (string, error) {
	aead, err := secretAEAD(true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := aead.Seal(nonce, nonce, []byte(plain), []byte(context))
	return sealPrefix + base64.RawStdEncoding.EncodeToString(out), nil
}

var errSecretUnreadable = errors.New("a stored secret can't be decrypted (was " + secretKeyFile + " replaced or lost?)")

// openSecret decrypts what sealSecret sealed for context.
func openSecret(context, sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, sealPrefix)
	if !ok {
		return "", errSecretUnreadable
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", errSecretUnreadable
	}
	aead, err := secretAEAD(false)
	if err != nil {
		return "", err
	}
	if len(data) < aead.NonceSize() {
		return "", errSecretUnreadable
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(context))
	if err != nil {
		return "", errSecretUnreadable
	}
	return string(plain), nil
}
