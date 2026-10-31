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

// encryptSecret seals secret with AES-256-GCM; the output layout is
// [12-byte nonce][ciphertext+tag]. A random nonce per write means two equal
// secrets never produce equal ciphertext.
func (s *Service) encryptSecret(targetID, secret string) ([]byte, error) {
	if !s.secretReady() {
		return nil, secretNotConfigured()
	}
	block, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
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
// The plaintext is only ever held in memory for one probe.
func (s *Service) decryptSecret(targetID string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if !s.secretReady() {
		return "", errors.New("deployment target secret cannot be decrypted: secret storage is not configured")
	}
	block, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", fmt.Errorf("deployment target secret is malformed")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], secretAAD(targetID))
	if err != nil {
		return "", errors.New("deployment target secret cannot be decrypted")
	}
	return string(plain), nil
}
