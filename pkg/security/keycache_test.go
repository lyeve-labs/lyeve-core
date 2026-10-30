package security

import (
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearKeyCache empties the process-wide cache, which is what a restart does.
func clearKeyCache(t testing.TB) {
	t.Helper()
	derivedKeys.mu.Lock()
	defer derivedKeys.mu.Unlock()
	clear(derivedKeys.entries)
	derivedKeys.order = derivedKeys.order[:0]
	derivedKeys.next = 0
}

func cachedKey(passphrase string, salt []byte) ([]byte, bool) {
	return derivedKeys.get(keyCacheID(passphrase, salt))
}

func cacheLen() int {
	derivedKeys.mu.RLock()
	defer derivedKeys.mu.RUnlock()
	return len(derivedKeys.entries)
}

func testID(i int) [32]byte {
	var id [32]byte
	copy(id[:], fmt.Sprintf("id-%d", i))
	return id
}

func TestKeyCache_EvictsTheOldestEntry(t *testing.T) {
	c := newKeyCache(3)
	for i := range 4 {
		c.put(testID(i), []byte(fmt.Sprintf("key-%d", i)))
	}
	_, ok := c.get(testID(0))
	assert.False(t, ok, "the first entry is evicted by the fourth")
	for i := 1; i < 4; i++ {
		key, ok := c.get(testID(i))
		require.True(t, ok, "entry %d", i)
		assert.Equal(t, fmt.Sprintf("key-%d", i), string(key[:5]))
	}

	c.put(testID(1), []byte("other"))
	key, _ := c.get(testID(1))
	assert.Equal(t, "key-1", string(key[:5]), "a second put keeps the first key and its place")

	c.put(testID(4), []byte("key-4"))
	_, ok = c.get(testID(1))
	assert.False(t, ok, "the oldest entry goes next")
	assert.Len(t, c.entries, 3)
	assert.Len(t, c.order, 3)
}

func TestKeyCache_IDSeparatesPassphrasesAndSalts(t *testing.T) {
	salt := make([]byte, saltSize)
	otherSalt := make([]byte, saltSize)
	otherSalt[0] = 1
	assert.NotEqual(t, keyCacheID("pass-a", salt), keyCacheID("pass-b", salt))
	assert.NotEqual(t, keyCacheID("pass-a", salt), keyCacheID("pass-a", otherSalt))
	id := keyCacheID("pass-a", salt)
	assert.NotContains(t, string(id[:]), "pass-a")
}

// A decrypt that derived its key would succeed after the cached key is
// replaced. One that read the cache fails, which shows it derived nothing.
func TestDecrypt_SecondReadUsesTheCachedKey(t *testing.T) {
	const pass = "cache-passphrase"
	cases := []struct {
		name    string
		encrypt func(t *testing.T) []byte
		decrypt func(raw []byte) (string, error)
	}{
		{"DecryptSecret", encryptSecretRaw, func(raw []byte) (string, error) {
			return DecryptSecret(hex.EncodeToString(raw), pass)
		}},
		{"DecryptSecretStrict", encryptSecretRaw, func(raw []byte) (string, error) {
			return DecryptSecretStrict(hex.EncodeToString(raw), pass)
		}},
		{"DecryptBytes", func(t *testing.T) []byte {
			raw, err := EncryptBytes([]byte("value"), pass)
			require.NoError(t, err)
			return raw
		}, func(raw []byte) (string, error) {
			out, err := DecryptBytes(raw, pass)
			return string(out), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.encrypt(t)
			salt := raw[:saltSize]
			_, seeded := cachedKey(pass, salt)
			require.True(t, seeded, "encrypting seeds the cache")

			clearKeyCache(t)
			got, err := tc.decrypt(raw)
			require.NoError(t, err)
			assert.Equal(t, "value", got)
			key, ok := cachedKey(pass, salt)
			require.True(t, ok, "the first decrypt caches its key")
			assert.Equal(t, derivePBKDF2Key(pass, salt), key)

			derivedKeys.mu.Lock()
			derivedKeys.entries[keyCacheID(pass, salt)] = [32]byte{}
			derivedKeys.mu.Unlock()
			_, err = tc.decrypt(raw)
			assert.Error(t, err, "the second decrypt reads the cached key")
		})
	}
}

func encryptSecretRaw(t *testing.T) []byte {
	enc, err := EncryptSecret("value", "cache-passphrase")
	require.NoError(t, err)
	raw, err := hex.DecodeString(enc)
	require.NoError(t, err)
	return raw
}

func TestDecrypt_WrongPassphraseFailsAndCachesNothing(t *testing.T) {
	enc, err := EncryptSecret("value", "right-passphrase")
	require.NoError(t, err)
	raw, err := hex.DecodeString(enc)
	require.NoError(t, err)
	salt := raw[:saltSize]
	_, ok := cachedKey("right-passphrase", salt)
	require.True(t, ok)
	before := cacheLen()

	_, err = DecryptSecret(enc, "wrong-passphrase")
	assert.Error(t, err)
	_, err = DecryptSecretStrict(enc, "wrong-passphrase")
	assert.Error(t, err)
	_, err = DecryptBytes(raw, "wrong-passphrase")
	assert.Error(t, err)

	_, ok = cachedKey("wrong-passphrase", salt)
	assert.False(t, ok, "a key that opened nothing is not cached")
	assert.Equal(t, before, cacheLen())
}

func TestDecryptSecret_ConcurrentReadersShareOneEntryPerValue(t *testing.T) {
	const pass = "concurrent-passphrase"
	encs := make([]string, 3)
	for i := range encs {
		enc, err := EncryptSecret(fmt.Sprintf("value-%d", i), pass)
		require.NoError(t, err)
		encs[i] = enc
	}
	clearKeyCache(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i, enc := range encs {
				got, err := DecryptSecret(enc, pass)
				if assert.NoError(t, err) {
					assert.Equal(t, fmt.Sprintf("value-%d", i), got)
				}
			}
		})
	}
	wg.Wait()

	assert.Equal(t, len(encs), cacheLen(), "concurrent misses leave one entry per value")
	derivedKeys.mu.RLock()
	assert.Len(t, derivedKeys.order, len(encs))
	derivedKeys.mu.RUnlock()
}

func TestKeyCache_ConcurrentPutAndGetStayBounded(t *testing.T) {
	c := newKeyCache(8)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 1000 {
				id := testID((g*1000 + i) % 50)
				c.put(id, id[:])
				if key, ok := c.get(id); ok {
					assert.Equal(t, id[:], key)
				}
			}
		})
	}
	wg.Wait()

	assert.Len(t, c.entries, 8)
	assert.Len(t, c.order, 8)
	for _, id := range c.order {
		_, ok := c.entries[id]
		assert.True(t, ok, "every id in the ring has its entry")
	}
}

func benchmarkDecryptTwiceAfterRestart(b *testing.B, n int) {
	const pass = "bench-passphrase"
	encs := make([]string, n)
	for i := range encs {
		enc, err := EncryptSecret(fmt.Sprintf("value-%d", i), pass)
		if err != nil {
			b.Fatal(err)
		}
		encs[i] = enc
	}
	for b.Loop() {
		b.StopTimer()
		clearKeyCache(b)
		b.StartTimer()
		for range 2 {
			for _, enc := range encs {
				if _, err := DecryptSecret(enc, pass); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
}

func BenchmarkDecryptSecret_OneValueTwice(b *testing.B) {
	benchmarkDecryptTwiceAfterRestart(b, 1)
}

func BenchmarkDecryptSecret_100ValuesTwice(b *testing.B) {
	benchmarkDecryptTwiceAfterRestart(b, 100)
}
