package cache

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/hashicorp/consul/lib"
)

// UpdateEvent is a struct summarising an update to a cache entry
type UpdateEvent struct {
	// CorrelationID is used by the Notify API to allow correlation of updates
	// with specific requests. We could return the full request object and
	// cachetype for consumers to match against the calls they made but in
	// practice it's cleaner for them to choose the minimal necessary unique
	// identifier given the set of things they are watching. They might even
	// choose to assign random IDs for example.
	CorrelationID string
	Result        interface{}
	Meta          ResultMeta
	Err           error
}

// Notify registers a desire to be updated about changes to a cache result.
//
// It is a helper that abstracts code from performing their own "blocking" query
// logic against a cache key to watch for changes and to maintain the key in
// cache actively. It will continue to perform blocking Get requests until the
// context is canceled.
//
// The passed context must be cancelled or timeout in order to free resources
// and stop maintaining the value in cache. Typically request-scoped resources
// do this but if a long-lived context like context.Background is used, then the
// caller must arrange for it to be cancelled when the watch is no longer
// needed.
//
// The passed chan may be buffered or unbuffered, if the caller doesn't consume
// fast enough it will block the notification loop. When the chan is later
// drained, watching resumes correctly. If the pause is longer than the
// cachetype's TTL, the result might be removed from the local cache. Even in
// this case though when the chan is drained again, the new Get will re-fetch
// the entry from servers and resume notification behavior transparently.
//
// The chan is passed in to allow multiple cached results to be watched by a
// single consumer without juggling extra goroutines per watch. The
// correlationID is opaque and will be returned in all UpdateEvents generated by
// result of watching the specified request so the caller can set this to any
// value that allows them to disambiguate between events in the returned chan
// when sharing a chan between multiple cache entries. If the chan is closed,
// the notify loop will terminate.
func (c *Cache) Notify(ctx context.Context, t string, r Request,
	correlationID string, ch chan<- UpdateEvent) error {

	// Get the type that we're fetching
	c.typesLock.RLock()
	tEntry, ok := c.types[t]
	c.typesLock.RUnlock()
	if !ok {
		return fmt.Errorf("unknown type in cache: %s", t)
	}
	if tEntry.Type.SupportsBlocking() {
		go c.notifyBlockingQuery(ctx, t, r, correlationID, ch)
	} else {
		info := r.CacheInfo()
		if info.MaxAge == 0 {
			return fmt.Errorf("Cannot use Notify for polling cache types without specifying the MaxAge")
		}
		go c.notifyPollingQuery(ctx, t, r, correlationID, ch, info.MaxAge)
	}

	return nil
}

func (c *Cache) notifyBlockingQuery(ctx context.Context, t string, r Request, correlationID string, ch chan<- UpdateEvent) {
	// Always start at 0 index to deliver the initial (possibly currently cached
	// value).
	index := uint64(0)
	failures := uint(0)

	for {
		// Check context hasn't been cancelled
		if ctx.Err() != nil {
			return
		}

		// Blocking request
		res, meta, err := c.getWithIndex(t, r, index)

		// Check context hasn't been cancelled
		if ctx.Err() != nil {
			return
		}

		// Check the index of the value returned in the cache entry to be sure it
		// changed
		if index < meta.Index {
			u := UpdateEvent{correlationID, res, meta, err}
			select {
			case ch <- u:
			case <-ctx.Done():
				return
			}

			// Update index for next request
			index = meta.Index
		}

		// Handle errors with backoff. Badly behaved blocking calls that returned
		// a zero index are considered as failures since we need to not get stuck
		// in a busy loop.
		if err == nil && meta.Index > 0 {
			failures = 0
		} else {
			failures++
		}
		if wait := backOffWait(failures); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		// Sanity check we always request blocking on second pass
		if index < 1 {
			index = 1
		}
	}
}

func (c *Cache) notifyPollingQuery(ctx context.Context, t string, r Request, correlationID string, ch chan<- UpdateEvent, maxAge time.Duration) {
	index := uint64(0)
	failures := uint(0)

	var lastValue interface{} = nil

	for {
		// Check context hasn't been cancelled
		if ctx.Err() != nil {
			return
		}

		// Make the request
		res, meta, err := c.getWithIndex(t, r, index)

		// Check context hasn't been cancelled
		if ctx.Err() != nil {
			return
		}

		// Check for a change in the value or an index change
		if index < meta.Index || !reflect.DeepEqual(lastValue, res) {
			u := UpdateEvent{correlationID, res, meta, err}
			select {
			case ch <- u:
			case <-ctx.Done():
				return
			}

			// Update index and lastValue
			lastValue = res
			index = meta.Index
		}

		// Reset or increment failure counter
		if err == nil {
			failures = 0
		} else {
			failures++
		}

		// Determining how long to wait before the next poll is complicated.
		// First off the happy path and the error path waits are handled distinctly
		//
		// Once fetching the data through the cache returns an error (and until a
		// non-error value is returned) the wait time between each round of the loop
		// gets controlled by the backOffWait function. Because we would have waited
		// at least until the age of the cached data was too old the error path should
		// immediately retry the fetch and backoff on the time as needed for persistent
		// failures which potentially will wait much longer than the MaxAge of the request
		//
		// When on the happy path we just need to fetch from the cache often enough to ensure
		// that the data is not older than the MaxAge. Therefore after fetching the data from
		// the cache we can sleep until the age of that data would exceed the MaxAge. Sometimes
		// this will be for the MaxAge duration (like when only a single notify was executed so
		// only 1 go routine is keeping the cache updated). Other times this will be some smaller
		// duration than MaxAge (when multiple notify calls were executed and this go routine just
		// got data back from the cache that was a cache hit after the other go routine fetched it
		// without a hit). We cannot just set MustRevalidate on the request and always sleep for MaxAge
		// as this would eliminate the single-flighting of these requests in the cache and
		// the efficiencies gained by it.
		if failures > 0 {

			errWait := backOffWait(failures)
			select {
			case <-time.After(errWait):
			case <-ctx.Done():
				return
			}
		} else {
			// Default to immediately re-poll. This only will happen if the data
			// we just got out of the cache is already too stale
			pollWait := 0 * time.Second

			// Calculate when the cached data's Age will get too stale and
			// need to be re-queried. When the data's Age already exceeds the
			// maxAge the pollWait value is left at 0 to immediately re-poll
			if meta.Age <= maxAge {
				pollWait = maxAge - meta.Age
			}

			// Add a small amount of random jitter to the polling time. One
			// purpose of the jitter is to ensure that the next time
			// we fetch from the cache the data will be stale (unless another
			// notify go routine has updated it while this one is sleeping).
			// Without this it would be possible to wake up, fetch the data
			// again where the age of the data is strictly equal to the MaxAge
			// and then immediately have to re-fetch again. That wouldn't
			// be terrible but it would expend a bunch more cpu cycles when
			// we can definitely avoid it.
			pollWait += lib.RandomStagger(maxAge / 16)

			select {
			case <-time.After(pollWait):
			case <-ctx.Done():
				return
			}
		}
	}
}
