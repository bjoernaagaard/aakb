package archive

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	maxConcurrentFileStats = 4
	fileStatsBudget        = 5 * time.Second
	fileStatsCacheTTL      = time.Hour
	fileStatsCacheCapacity = 1024
)

// MD5Summary is the JSON response from /dyn/md5/summary/<hash>?cachebust=<ts>.
type MD5Summary struct {
	ReportsCount        int `json:"reports_count"`
	CommentsCount       int `json:"comments_count"`
	ListsCount          int `json:"lists_count"`
	DownloadsTotal      int `json:"downloads_total"`
	GreatQualityCount   int `json:"great_quality_count"`
	DownloadsLeft       int `json:"downloads_left"`
	IsMember            int `json:"is_member"`
	DownloadStillActive int `json:"download_still_active"`
}

// MD5InlineInfo is the public counter response from /dyn/md5/inline_info/<hash>.
type MD5InlineInfo struct {
	ReportsCount      int `json:"reports_count"`
	CommentsCount     int `json:"comments_count"`
	ListsCount        int `json:"lists_count"`
	DownloadsTotal    int `json:"downloads_total"`
	GreatQualityCount int `json:"great_quality_count"`
}

// UnmarshalJSON requires all five nonnegative public counter fields to be present.
func (info *MD5InlineInfo) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("statistics response must be an object")
	}
	var parsed MD5InlineInfo
	required := []struct {
		name string
		dest *int
	}{
		{"reports_count", &parsed.ReportsCount},
		{"comments_count", &parsed.CommentsCount},
		{"lists_count", &parsed.ListsCount},
		{"downloads_total", &parsed.DownloadsTotal},
		{"great_quality_count", &parsed.GreatQualityCount},
	}
	for _, field := range required {
		raw, ok := fields[field.name]
		if !ok {
			return fmt.Errorf("statistics response is missing %s", field.name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("statistics field %s must be an integer", field.name)
		}
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("statistics field %s must be an integer: %w", field.name, err)
		}
		if value < 0 {
			return fmt.Errorf("statistics field %s must not be negative", field.name)
		}
		*field.dest = value
	}
	*info = parsed
	return nil
}

// FetchSummary retrieves /dyn/md5/summary/<hash>?cachebust=<ts> from base.
func (c *Client) FetchSummary(ctx context.Context, base, hash string) (*MD5Summary, error) {
	summaryURL := buildPathURL(base, "/dyn/md5/summary/"+hash) + "?cachebust=" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var summary MD5Summary
	if err := c.fetchJSON(ctx, summaryURL, &summary, "file statistics"); err != nil {
		return nil, err
	}
	return &summary, nil
}

// FileStats fetches public counters for valid MD5 hashes using a 1024-entry
// 1-hour LRU cache, a 4-worker bounded pool, and a 5-second total batch budget.
func (c *Client) FileStats(ctx context.Context, hashes []string) (map[string]RecordStats, error) {
	results := make(map[string]RecordStats)
	if err := ctx.Err(); err != nil {
		return results, err
	}

	seen := make(map[string]struct{}, len(hashes))
	pending := make([]string, 0, len(hashes))
	for _, rawHash := range hashes {
		hash, err := normalizeHash(rawHash)
		if err != nil {
			continue
		}
		if _, exists := seen[hash]; exists {
			continue
		}
		seen[hash] = struct{}{}
		if stats, ok := c.fileStatsCache.get(hash); ok {
			results[hash] = stats
			continue
		}
		pending = append(pending, hash)
	}
	if len(pending) == 0 {
		return results, ctx.Err()
	}

	budget := c.fileStatsTimeout
	if budget <= 0 {
		budget = fileStatsBudget
	}
	requestCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	workerCount := min(maxConcurrentFileStats, len(pending))
	jobs := make(chan string)
	var workers sync.WaitGroup
	var resultMu sync.Mutex
	for range workerCount {
		workers.Go(func() {
			for hash := range jobs {
				stats, err := c.fetchFileStats(requestCtx, hash)
				if err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						c.log.WarnContext(requestCtx, "could not fetch public file statistics", "hash", hash, "error", err)
					}
					continue
				}
				resultMu.Lock()
				results[hash] = stats
				resultMu.Unlock()
			}
		})
	}

dispatch:
	for _, hash := range pending {
		select {
		case jobs <- hash:
		case <-requestCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()

	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func (c *Client) fetchFileStats(ctx context.Context, hash string) (RecordStats, error) {
	select {
	case c.fileStatsSlots <- struct{}{}:
	case <-ctx.Done():
		return RecordStats{}, ctx.Err()
	}
	defer func() { <-c.fileStatsSlots }()

	if stats, ok := c.fileStatsCache.get(hash); ok {
		return stats, nil
	}

	var stats RecordStats
	err := c.onMirror(ctx, func(base string) error {
		var response MD5InlineInfo
		rawURL := buildPathURL(base, "/dyn/md5/inline_info/"+hash)
		headers := http.Header{"Accept": {"text/css"}}
		if err := c.fetchJSONWithHeaders(ctx, rawURL, &response, "public file statistics", headers); err != nil {
			return err
		}
		stats = RecordStats{
			Downloads:    response.DownloadsTotal,
			GreatQuality: response.GreatQualityCount,
			Reports:      response.ReportsCount,
			Lists:        response.ListsCount,
			Comments:     response.CommentsCount,
		}
		return nil
	})
	if err != nil {
		return RecordStats{}, err
	}
	c.fileStatsCache.put(hash, stats)
	return stats, nil
}

type statsCacheEntry struct {
	hash      string
	stats     RecordStats
	expiresAt time.Time
}

type statsCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

func newStatsCache() statsCache {
	return statsCache{
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		ttl:        fileStatsCacheTTL,
		maxEntries: fileStatsCacheCapacity,
		now:        time.Now,
	}
}

func (cache *statsCache) get(hash string) (RecordStats, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.entries[hash]
	if !ok {
		return RecordStats{}, false
	}
	entry, ok := element.Value.(*statsCacheEntry)
	if !ok {
		cache.order.Remove(element)
		delete(cache.entries, hash)
		return RecordStats{}, false
	}
	if !cache.now().Before(entry.expiresAt) {
		cache.remove(element)
		return RecordStats{}, false
	}
	cache.order.MoveToFront(element)
	return entry.stats, true
}

func (cache *statsCache) put(hash string, stats RecordStats) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	if element, ok := cache.entries[hash]; ok {
		if entry, valid := element.Value.(*statsCacheEntry); valid {
			entry.stats = stats
			entry.expiresAt = now.Add(cache.ttl)
			cache.order.MoveToFront(element)
			return
		}
		cache.remove(element)
	}
	if cache.maxEntries <= 0 {
		return
	}
	entry := &statsCacheEntry{
		hash:      hash,
		stats:     stats,
		expiresAt: now.Add(cache.ttl),
	}
	cache.entries[hash] = cache.order.PushFront(entry)
	for cache.order.Len() > cache.maxEntries {
		cache.remove(cache.order.Back())
	}
}

func (cache *statsCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	if entry, ok := element.Value.(*statsCacheEntry); ok {
		delete(cache.entries, entry.hash)
	}
	cache.order.Remove(element)
}
