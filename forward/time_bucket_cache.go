package forward

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// TimeBucketCache implements an efficient time-based cache using bucketing
type TimeBucketCache struct {
	buckets         []*timeBucket       // Time-ordered buckets
	bucketMap       map[common.Hash]int // hash -> bucket index for O(1) lookup
	mutex           sync.RWMutex
	bucketSize      time.Duration // Duration each bucket represents
	maxBuckets      int           // Maximum number of buckets to keep
	ttl             time.Duration // Time-to-live for entries
	startTime       time.Time     // Reference start time for bucket calculation
	cleanupBuffer   []common.Hash // Reusable buffer for cleanup operations
	memoryThreshold int           // Memory threshold to trigger cleanup
	lastCleanupTime time.Time     // Track last cleanup time
	cleanupCount    int           // Count of cleanup operations
}

// timeBucket represents a time bucket containing hashes for a specific time period
type timeBucket struct {
	bucketTime time.Time                // Start time of this bucket
	hashes     map[common.Hash]struct{} // Set of hashes in this bucket
}

// NewTimeBucketCache creates a new time-bucket based cache
func NewTimeBucketCache(bucketSize, ttl time.Duration) *TimeBucketCache {
	maxBuckets := int(ttl/bucketSize) + 2 // +2 for safety buffer
	memoryThreshold := maxBuckets * 100   // Estimated threshold based on max buckets
	return &TimeBucketCache{
		buckets:         make([]*timeBucket, 0, maxBuckets),
		bucketMap:       make(map[common.Hash]int, 1000), // Pre-allocate for common case
		bucketSize:      bucketSize,
		maxBuckets:      maxBuckets,
		ttl:             ttl,
		startTime:       time.Now().Truncate(bucketSize),
		cleanupBuffer:   make([]common.Hash, 0, 500), // Pre-allocated cleanup buffer
		memoryThreshold: memoryThreshold,
		lastCleanupTime: time.Now(),
	}
}

// getBucketIndex calculates which bucket a timestamp belongs to
func (c *TimeBucketCache) getBucketIndex(t time.Time) int {
	if t.Before(c.startTime) {
		return -1
	}
	index := int(t.Sub(c.startTime) / c.bucketSize)
	// Ensure we don't return an extremely large index due to time drift
	if index > c.maxBuckets*2 {
		return c.maxBuckets * 2
	}
	return index
}

// ensureBucket ensures a bucket exists for the given time
func (c *TimeBucketCache) ensureBucket(t time.Time) int {
	bucketIndex := c.getBucketIndex(t)

	if bucketIndex < 0 {
		return -1
	}

	if bucketIndex >= c.maxBuckets*2 {
		// Trigger cleanup and recalculate
		c.cleanupExpiredBuckets()
		bucketIndex = c.getBucketIndex(t)
		if bucketIndex >= c.maxBuckets*2 {
			return -1
		}
	}

	// Batch extend buckets slice if needed for better performance
	if len(c.buckets) <= bucketIndex {
		newSize := bucketIndex + 1
		if cap(c.buckets) < newSize {
			// Grow capacity by 1.5x or to required size, whichever is larger
			newCap := max(newSize, cap(c.buckets)*3/2)
			newBuckets := make([]*timeBucket, len(c.buckets), newCap)
			copy(newBuckets, c.buckets)
			c.buckets = newBuckets
		}
		// Extend to required size
		for len(c.buckets) <= bucketIndex {
			c.buckets = append(c.buckets, nil)
		}
	}

	// Create bucket if it doesn't exist
	if c.buckets[bucketIndex] == nil {
		bucketTime := c.startTime.Add(time.Duration(bucketIndex) * c.bucketSize)
		c.buckets[bucketIndex] = &timeBucket{
			bucketTime: bucketTime,
			hashes:     make(map[common.Hash]struct{}, 50), // Pre-allocate for common case
		}
	}

	return bucketIndex
}

// Add adds a hash to the cache
func (c *TimeBucketCache) Add(hash common.Hash) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()

	// Remove from old bucket if exists
	if oldBucketIndex, exists := c.bucketMap[hash]; exists && oldBucketIndex < len(c.buckets) && c.buckets[oldBucketIndex] != nil {
		delete(c.buckets[oldBucketIndex].hashes, hash)
	}

	// Add to new bucket
	bucketIndex := c.ensureBucket(now)
	if bucketIndex == -1 {
		return
	}
	c.buckets[bucketIndex].hashes[hash] = struct{}{}
	c.bucketMap[hash] = bucketIndex

	// Trigger cleanup based on multiple conditions
	c.triggerCleanupIfNeeded()
}

// Contains checks if a hash exists in the cache (optimized for read-heavy workloads)
func (c *TimeBucketCache) Contains(hash common.Hash) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	bucketIndex, exists := c.bucketMap[hash]
	if !exists {
		return false
	}

	if bucketIndex >= len(c.buckets) {
		return false
	}

	bucket := c.buckets[bucketIndex]
	if bucket == nil {
		return false
	}

	_, exists = bucket.hashes[hash]
	return exists
}

// triggerCleanupIfNeeded checks if cleanup should be triggered based on various conditions
func (c *TimeBucketCache) triggerCleanupIfNeeded() {
	now := time.Now()
	// Trigger cleanup based on:
	// 1. Too many buckets
	// 2. Memory threshold exceeded
	// 3. Minimum time interval since last cleanup
	if len(c.buckets) > c.maxBuckets ||
		len(c.bucketMap) > c.memoryThreshold ||
		now.Sub(c.lastCleanupTime) > c.ttl/2 {
		c.cleanupExpiredBuckets()
		c.lastCleanupTime = now
		c.cleanupCount++
	}
}

// cleanupExpiredBuckets removes buckets older than TTL
func (c *TimeBucketCache) cleanupExpiredBuckets() {
	cutoff := time.Now().Add(-c.ttl)
	cutoffBucketIndex := c.getBucketIndex(cutoff)

	// Reuse cleanup buffer to avoid allocations
	c.cleanupBuffer = c.cleanupBuffer[:0]
	for i := 0; i <= cutoffBucketIndex && i < len(c.buckets); i++ {
		if c.buckets[i] != nil {
			for hash := range c.buckets[i].hashes {
				c.cleanupBuffer = append(c.cleanupBuffer, hash)
			}
			c.buckets[i] = nil
		}
	}

	for _, hash := range c.cleanupBuffer {
		delete(c.bucketMap, hash)
	}

	// Compact buckets slice by removing leading nil entries
	if cutoffBucketIndex >= 0 && cutoffBucketIndex < len(c.buckets)-1 {
		newStartIndex := cutoffBucketIndex + 1
		newLen := len(c.buckets) - newStartIndex

		// Move remaining buckets to front instead of creating new slice
		copy(c.buckets[0:newLen], c.buckets[newStartIndex:])
		// Clear the tail to avoid memory leaks
		for i := newLen; i < len(c.buckets); i++ {
			c.buckets[i] = nil
		}
		c.buckets = c.buckets[:newLen]

		// Update bucket indices in bucketMap for remaining entries
		for hash, oldIndex := range c.bucketMap {
			newIndex := oldIndex - newStartIndex
			if newIndex >= 0 {
				c.bucketMap[hash] = newIndex
			} else {
				delete(c.bucketMap, hash)
			}
		}

		c.startTime = c.startTime.Add(time.Duration(newStartIndex) * c.bucketSize)
	} else if cutoffBucketIndex >= len(c.buckets)-1 {
		// All buckets are expired
		c.buckets = c.buckets[:0]
		for k := range c.bucketMap {
			delete(c.bucketMap, k)
		}
		c.startTime = time.Now().Truncate(c.bucketSize)
	}
}

// Cleanup performs manual cleanup of expired entries
func (c *TimeBucketCache) Cleanup() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.cleanupExpiredBuckets()
}

// Size returns the current number of entries in the cache
func (c *TimeBucketCache) Size() int {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return len(c.bucketMap)
}
