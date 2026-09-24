//go:build linux

package template

import (
	"context"
	"testing"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCache(defaultTTL time.Duration) *Cache {
	return &Cache{
		cache: ttlcache.New(ttlcache.WithTTL[string, Template](defaultTTL)),
	}
}

// simulateGetTemplate runs getTemplateWithFetch's TTL computation and admission
// without needing a full storageTemplate (which requires disk paths).
func simulateGetTemplate(c *Cache, key string, maxSandboxLengthHours int64) {
	ttl := templateExpiration
	if maxSandboxLengthHours > 0 {
		ttl = max(ttl, time.Duration(maxSandboxLengthHours)*time.Hour+templateExpirationBuffer)
	}

	_, _, release := c.lookupOrAdmit(context.Background(), key, nil, ttl, false)
	release()
}

func TestGetTemplate_ExtendsTTL(t *testing.T) {
	t.Parallel()

	defaultTTL := 50 * time.Millisecond
	c := newTestCache(defaultTTL)
	go c.cache.Start()
	defer c.cache.Stop()

	key := "build-long-running"
	c.cache.Set(key, nil, defaultTTL)

	simulateGetTemplate(c, key, 168)

	item := c.cache.Get(key)
	require.NotNil(t, item)
	assert.Equal(t, 168*time.Hour+templateExpirationBuffer, item.TTL())

	time.Sleep(defaultTTL + 20*time.Millisecond)

	item = c.cache.Get(key)
	assert.NotNil(t, item, "entry must survive past the original default TTL")
}

func TestGetTemplate_NeverShortens(t *testing.T) {
	t.Parallel()

	c := newTestCache(time.Hour)
	key := "build-shared"

	simulateGetTemplate(c, key, 168)

	item := c.cache.Get(key)
	require.NotNil(t, item)
	longTTL := item.TTL()

	simulateGetTemplate(c, key, 24)

	item = c.cache.Get(key)
	require.NotNil(t, item)
	assert.Equal(t, longTTL, item.TTL(), "TTL must not decrease when a shorter team accesses the template")
}

func TestGetTemplate_HitRestartsExpiry(t *testing.T) {
	t.Parallel()

	c := newTestCache(time.Hour)
	key := "build-touch"
	before := c.cache.Set(key, nil, time.Hour).ExpiresAt()

	time.Sleep(5 * time.Millisecond)

	_, found, release := c.lookupOrAdmit(t.Context(), key, nil, time.Hour, false)
	release()
	require.True(t, found)

	item := c.cache.Get(key, ttlcache.WithDisableTouchOnHit[string, Template]())
	require.NotNil(t, item)
	assert.True(t, item.ExpiresAt().After(before), "a hit must restart the entry's expiry")
}

func TestGetTemplate_DefaultTTLForZero(t *testing.T) {
	t.Parallel()

	c := newTestCache(time.Hour)
	key := "build-default"

	simulateGetTemplate(c, key, 0)

	item := c.cache.Get(key)
	require.NotNil(t, item)
	assert.Equal(t, templateExpiration, item.TTL())
}

func TestGetTemplate_SetDoesNotTriggerOnEviction(t *testing.T) {
	t.Parallel()

	inner := ttlcache.New(ttlcache.WithTTL[string, Template](time.Hour))

	evicted := false
	inner.OnEviction(func(_ context.Context, _ ttlcache.EvictionReason, _ *ttlcache.Item[string, Template]) {
		evicted = true
	})

	c := &Cache{cache: inner}

	key := "build-1"
	c.cache.Set(key, nil, ttlcache.DefaultTTL)
	simulateGetTemplate(c, key, 168)

	assert.False(t, evicted, "Set() on existing key must NOT trigger OnEviction")

	item := c.cache.Get(key)
	require.NotNil(t, item)
	assert.Equal(t, 168*time.Hour+templateExpirationBuffer, item.TTL())
}

func TestWithoutExtend_EntryEvictedEarly(t *testing.T) {
	t.Parallel()

	defaultTTL := 50 * time.Millisecond
	c := newTestCache(defaultTTL)
	go c.cache.Start()
	defer c.cache.Stop()

	key := "build-will-expire"
	c.cache.Set(key, nil, defaultTTL)

	time.Sleep(defaultTTL + 30*time.Millisecond)

	item := c.cache.Get(key)
	assert.Nil(t, item, "without TTL extension, the entry should be evicted after the default TTL")
}
