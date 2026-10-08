package agentd

import (
	"strconv"
	"strings"
	"sync"

	"github.com/zzjcool/homer-cli/internal/core"
)

// snapshotCache keeps one decoded hub generation in memory. The epoch guards
// against an in-flight GET repopulating the cache after an upload invalidates
// it (or overwriting a newer response from another concurrent GET).
type snapshotCache struct {
	mu        sync.RWMutex
	etag      string
	snapshots []core.AdapterSnapshot
	meta      []byte
	epoch     uint64
	valid     bool
	flight    *snapshotDownloadFlight
}

type snapshotDownloadFlight struct {
	epoch     uint64
	done      chan struct{}
	snapshots []core.AdapterSnapshot
	meta      []byte
	err       error
}

func (c *snapshotCache) beginDownload() (*snapshotDownloadFlight, bool) {
	if c == nil {
		return nil, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flight != nil && c.flight.epoch == c.epoch {
		return c.flight, false
	}
	flight := &snapshotDownloadFlight{epoch: c.epoch, done: make(chan struct{})}
	c.flight = flight
	return flight, true
}

func (c *snapshotCache) finishDownload(flight *snapshotDownloadFlight, snapshots []core.AdapterSnapshot, meta []byte, err error) {
	if flight == nil {
		return
	}
	if c != nil {
		c.mu.Lock()
	}
	flight.snapshots = cloneAdapterSnapshots(snapshots)
	flight.meta = append([]byte(nil), meta...)
	flight.err = err
	if c != nil && c.flight == flight {
		c.flight = nil
	}
	close(flight.done)
	if c != nil {
		c.mu.Unlock()
	}
}

func (c *snapshotCache) requestState() (etag string, epoch uint64, valid bool) {
	if c == nil {
		return "", 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.etag, c.epoch, c.valid && c.etag != ""
}

func (c *snapshotCache) get(etag string, epoch uint64) ([]core.AdapterSnapshot, []byte, bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.valid || c.epoch != epoch || c.etag != etag {
		return nil, nil, false
	}
	return cloneAdapterSnapshots(c.snapshots), append([]byte(nil), c.meta...), true
}

func (c *snapshotCache) store(epoch uint64, etag string, snapshots []core.AdapterSnapshot, meta []byte) uint64 {
	if c == nil {
		return epoch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return c.epoch
	}
	// An absent ETag cannot be used for conditional requests. Clear any
	// previous generation so it cannot be mistaken for the response body.
	if etag == "" {
		c.clearLocked()
		return c.epoch
	}
	if c.valid {
		oldGeneration, oldOK := generationFromETag(c.etag)
		newGeneration, newOK := generationFromETag(etag)
		// Concurrent GETs can complete out of order. Do not let a response
		// for an older g<N> replace a newer decoded generation.
		if oldOK && newOK && newGeneration < oldGeneration {
			return c.epoch
		}
	}
	c.etag = etag
	c.snapshots = cloneAdapterSnapshots(snapshots)
	c.meta = append([]byte(nil), meta...)
	c.valid = true
	return c.epoch
}

func (c *snapshotCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.clearLocked()
	c.mu.Unlock()
}

func (c *snapshotCache) invalidateIf(etag string, epoch uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.valid && c.etag == etag && c.epoch == epoch {
		c.clearLocked()
	}
	c.mu.Unlock()
}

func generationFromETag(etag string) (int64, bool) {
	etag = strings.TrimSpace(etag)
	etag = strings.Trim(etag, `"`)
	if !strings.HasPrefix(etag, "g") {
		return 0, false
	}
	generation, err := strconv.ParseInt(strings.TrimPrefix(etag, "g"), 10, 64)
	return generation, err == nil && generation >= 0
}

func (c *snapshotCache) clearLocked() {
	c.etag = ""
	c.snapshots = nil
	c.meta = nil
	c.valid = false
	c.epoch++
}

func cloneAdapterSnapshots(snapshots []core.AdapterSnapshot) []core.AdapterSnapshot {
	if snapshots == nil {
		return nil
	}
	out := make([]core.AdapterSnapshot, len(snapshots))
	for i, adapter := range snapshots {
		out[i].AdapterID = adapter.AdapterID
		if adapter.Categories == nil {
			continue
		}
		out[i].Categories = make([]core.CategorySnapshot, len(adapter.Categories))
		for j, category := range adapter.Categories {
			out[i].Categories[j] = category
			if category.Files != nil {
				out[i].Categories[j].Files = make(core.SnapshotFiles, len(category.Files))
				for path, entry := range category.Files {
					out[i].Categories[j].Files[path] = entry
				}
			}
		}
	}
	return out
}
