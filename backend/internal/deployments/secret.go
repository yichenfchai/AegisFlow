package deployments

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// secretAADPrefix binds a ciphertext to the target it belongs to, so a sealed
// value copied to another row cannot be decrypted there.
const secretAADPrefix = "deployment_target:"

func secretAAD(targetID string) []byte { return []byte(secretAADPrefix + targetID) }

// newGCM builds the AEAD for one key.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// openSealed opens one ciphertext; callers check the length first.
func openSealed(aead cipher.AEAD, targetID string, sealed []byte) ([]byte, error) {
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], secretAAD(targetID))
}

// encryptSecret seals secret with AES-256-GCM; the output layout is
// [12-byte nonce][ciphertext+tag]. A random nonce per write means two equal
// secrets never produce equal ciphertext. Only the current key ever seals, so
// a rotation never writes a credential that needs the retiring key.
func (s *Service) encryptSecret(targetID, secret string) ([]byte, error) {
	if !s.secretReady() {
		return nil, secretNotConfigured()
	}
	aead, err := newGCM(s.secretKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(secret), secretAAD(targetID)), nil
}

// decryptSecret opens a stored ciphertext for the target it was sealed for.
// The plaintext is only ever held in memory for one probe. When a rotation is
// in progress the previous key is tried after the current one, so a target
// keeps working without rewriting its credential; the failure is the same
// whichever key was tried, so the error cannot be used to probe which key
// sealed a row.
func (s *Service) decryptSecret(targetID string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if !s.secretReady() {
		return "", errors.New("deployment target secret cannot be decrypted: secret storage is not configured")
	}
	aead, err := newGCM(s.secretKey)
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", fmt.Errorf("deployment target secret is malformed")
	}
	if plain, openErr := openSealed(aead, targetID, sealed); openErr == nil {
		return string(plain), nil
	}
	if len(s.previousKey) == 32 {
		if previousAEAD, previousErr := newGCM(s.previousKey); previousErr == nil {
			if plain, openErr := openSealed(previousAEAD, targetID, sealed); openErr == nil {
				return string(plain), nil
			}
		}
	}
	return "", errors.New("deployment target secret cannot be decrypted")
}
