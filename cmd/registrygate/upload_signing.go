package main

import (
	"log/slog"
	"sync"

	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/uploadtoken"
)

// uploadSigningKeyField is the Secret key holding the upload signing key.
const uploadSigningKeyField = "key"

// uploadSigning reads the upload signing key from its Secret through the Secret store, so a rotated key applies
// without a restart.
type uploadSigning struct {
	store  *secrets.Store
	key    secrets.Key
	logger *slog.Logger

	mu              sync.Mutex
	resourceVersion string
	codec           *uploadtoken.Codec
}

// Codec returns the codec for the current key, or nil while the Secret is missing, pending or holds no usable key.
func (u *uploadSigning) Codec() *uploadtoken.Codec {
	secret, ok := u.store.Get(u.key)
	if !ok || secret.State != secrets.Present {
		return nil
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.resourceVersion == secret.ResourceVersion {
		return u.codec
	}

	codec, err := uploadtoken.New(secret.Data[uploadSigningKeyField])
	if err != nil {
		u.logger.Error("upload signing Secret holds no usable key", "secret", u.key.String(), "error", err.Error())
	}

	u.resourceVersion, u.codec = secret.ResourceVersion, codec

	return codec
}

// Loaded reports whether a usable key is loaded.
func (u *uploadSigning) Loaded() bool { return u.Codec() != nil }
