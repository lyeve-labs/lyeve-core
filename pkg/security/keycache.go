package security

import (
	"crypto/sha256"
	"sync"
)

// keyCacheSize bounds the derived-key cache. Each PBKDF2 derivation costs
// about 0.1 s of CPU, so a caller that decrypts a stored value on every read
// pays that per value per read. 4,096 entries hold the keys of a few
// thousand stored values, and every entry is a fixed 64 bytes plus a 32-byte
// slot in the eviction ring, so a full cache stays under 1 MiB.
const keyCacheSize = 4096

// derivedKeys holds the PBKDF2 keys this process has already derived, so a
// value is derived once per process instead of once per decrypt.
var derivedKeys = newKeyCache(keyCacheSize)

// keyCache maps a digest of (salt, passphrase) to the key PBKDF2 derives
// from them. When full it evicts the entry inserted first. Two callers that
// miss the same entry at once both derive, and the second put is a no-op:
// deduplicating would hold one caller behind the other for no saving.
type keyCache struct {
	mu      sync.RWMutex
	size    int
	entries map[[32]byte][32]byte
	order   [][32]byte // insertion order, a ring once len reaches size
	next    int        // the oldest slot of order once it is a ring
}

func newKeyCache(size int) *keyCache {
	return &keyCache{size: size, entries: make(map[[32]byte][32]byte)}
}

// keyCacheID names a cache entry without holding the passphrase. Every salt
// is saltSize bytes, so the concatenation is unambiguous, and two
// passphrases share an entry only through a SHA-256 collision.
func keyCacheID(passphrase string, salt []byte) [32]byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(passphrase))
	var id [32]byte
	h.Sum(id[:0])
	return id
}

func (c *keyCache) get(id [32]byte) ([]byte, bool) {
	c.mu.RLock()
	key, ok := c.entries[id]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return key[:], true
}

func (c *keyCache) put(id [32]byte, key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[id]; ok {
		return
	}
	if len(c.order) < c.size {
		c.order = append(c.order, id)
	} else {
		delete(c.entries, c.order[c.next])
		c.order[c.next] = id
		c.next = (c.next + 1) % c.size
	}
	var k [32]byte
	copy(k[:], key)
	c.entries[id] = k
}

// pbkdf2KeyFor derives the key for passphrase and salt and caches it, for an
// encrypt, whose key is right by construction.
func pbkdf2KeyFor(passphrase string, salt []byte) []byte {
	key := derivePBKDF2Key(passphrase, salt)
	derivedKeys.put(keyCacheID(passphrase, salt), key)
	return key
}

// openPBKDF2 decrypts (nonce || ciphertext+tag) with the key for passphrase
// and salt, deriving it only on a cache miss. A derived key is cached only
// once it has opened the value, so a wrong passphrase never takes an entry.
func openPBKDF2(passphrase string, salt, data []byte) ([]byte, error) {
	id := keyCacheID(passphrase, salt)
	key, cached := derivedKeys.get(id)
	if !cached {
		key = derivePBKDF2Key(passphrase, salt)
	}
	plaintext, err := decryptGCM(key, data)
	if err == nil && !cached {
		derivedKeys.put(id, key)
	}
	return plaintext, err
}
