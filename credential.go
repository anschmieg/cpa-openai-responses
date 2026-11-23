package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var credentialBucket = []byte("responses-credential-v1")
var credentialEntry = []byte("project-key")

func (cfg *config) setKey(key string) {
	cfg.apiKey = key
	cfg.namespace = sha256.Sum256([]byte(cfg.BaseURL + "\x00" + key))
}

func validKey(key string) bool {
	return key != "" && len(key) <= 512 && !strings.ContainsAny(key, " \r\n\t")
}

func credentialCipher(stateFile string, create bool) (cipher.AEAD, error) {
	const keySize = 32
	var key []byte
	var err error
	if encoded := os.Getenv("OPENAI_RESPONSES_STORAGE_KEY"); encoded != "" {
		key, err = base64.StdEncoding.DecodeString(encoded)
	} else {
		path := stateFile + ".key"
		key, err = os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && create {
			key = make([]byte, keySize)
			if _, err = rand.Read(key); err != nil {
				return nil, err
			}
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr != nil {
				return nil, openErr
			}
			_, writeErr := file.Write(key)
			err = errors.Join(writeErr, file.Sync(), file.Close())
			if err != nil {
				_ = os.Remove(path)
				return nil, err
			}
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("credential encryption key file is unavailable or has unsafe permissions")
		}
	}
	if err != nil || len(key) != keySize {
		return nil, errors.New("invalid credential encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func readCredential(db *bolt.DB, cfg config) (string, bool, error) {
	var encrypted []byte
	if err := db.View(func(tx *bolt.Tx) error {
		if bucket := tx.Bucket(credentialBucket); bucket != nil {
			encrypted = append(encrypted, bucket.Get(credentialEntry)...)
		}
		return nil
	}); err != nil || len(encrypted) == 0 {
		return "", false, err
	}
	box, err := credentialCipher(cfg.StateFile, false)
	if err != nil || len(encrypted) < box.NonceSize() {
		return "", true, errors.New("cannot open encrypted project credential")
	}
	data, err := box.Open(nil, encrypted[:box.NonceSize()], encrypted[box.NonceSize():], nil)
	if err != nil {
		return "", true, errors.New("cannot open encrypted project credential")
	}
	var saved struct{ BaseURL, Key string }
	if err := json.Unmarshal(data, &saved); err != nil || !validKey(saved.Key) {
		return "", true, errors.New("invalid encrypted project credential")
	}
	if saved.BaseURL != cfg.BaseURL {
		return "", true, nil
	}
	return saved.Key, true, nil
}

func writeCredential(db *bolt.DB, cfg config, key string) error {
	box, err := credentialCipher(cfg.StateFile, true)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct{ BaseURL, Key string }{cfg.BaseURL, key})
	if err != nil {
		return err
	}
	nonce := make([]byte, box.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	encrypted := box.Seal(nonce, nonce, data, nil)
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(credentialBucket)
		if err != nil {
			return err
		}
		return bucket.Put(credentialEntry, encrypted)
	})
}
