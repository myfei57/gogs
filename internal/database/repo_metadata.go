package database

import (
	"sync"

	log "unknwon.dev/clog/v2"
)

// RepoMetadataField identifies a piece of repository metadata whose committed
// change makes cached copies stale.
type RepoMetadataField uint8

// Fields of repository metadata whose changes produce an invalidation event.
const (
	RepoMetadataFieldName RepoMetadataField = 1 << iota
	RepoMetadataFieldVisibility
	RepoMetadataFieldDefaultBranch
	RepoMetadataFieldArchiveStatus
)

// String returns the names of the covered fields joined by a pipe.
func (f RepoMetadataField) String() string {
	if f == 0 {
		return "none"
	}

	known := []struct {
		bit  RepoMetadataField
		name string
	}{
		{RepoMetadataFieldName, "name"},
		{RepoMetadataFieldVisibility, "visibility"},
		{RepoMetadataFieldDefaultBranch, "default_branch"},
		{RepoMetadataFieldArchiveStatus, "archive_status"},
	}

	var names string
	for _, k := range known {
		if f&k.bit == 0 {
			continue
		}
		if names != "" {
			names += "|"
		}
		names += k.name
	}
	return names
}

// RepoMetadataInvalidationEvent tells consumers that cached metadata of a
// repository may be stale. Version is monotonically increasing within a
// repository and is persisted together with the change, so consumers can
// ignore duplicated or out-of-order events by comparing the version.
type RepoMetadataInvalidationEvent struct {
	RepoID  int64
	Version int64
	Fields  RepoMetadataField
}

// Has reports whether the event covers the given field.
func (e RepoMetadataInvalidationEvent) Has(field RepoMetadataField) bool {
	return e.Fields&field != 0
}

// repoMetadataEventBufferSize is the number of events a slow consumer can
// queue before further events for it are dropped. Consumers only receive an
// invalidation signal and can always recover by loading the latest state.
const repoMetadataEventBufferSize = 64

// repoMetadataEventBus fans committed repository metadata invalidation events
// out to in-process consumers.
type repoMetadataEventBus struct {
	mu          sync.RWMutex
	nextSubID   uint64
	subscribers map[uint64]chan RepoMetadataInvalidationEvent
}

func newRepoMetadataEventBus() *repoMetadataEventBus {
	return &repoMetadataEventBus{
		subscribers: make(map[uint64]chan RepoMetadataInvalidationEvent),
	}
}

var repoMetadataEvents = newRepoMetadataEventBus()

// SubscribeRepoMetadataInvalidation registers a consumer of repository
// metadata invalidation events. Events are delivered after the change is
// committed. The returned function unsubscribes the consumer and closes its
// channel.
func SubscribeRepoMetadataInvalidation() (<-chan RepoMetadataInvalidationEvent, func()) {
	return repoMetadataEvents.subscribe()
}

func (b *repoMetadataEventBus) subscribe() (<-chan RepoMetadataInvalidationEvent, func()) {
	ch := make(chan RepoMetadataInvalidationEvent, repoMetadataEventBufferSize)

	b.mu.Lock()
	b.nextSubID++
	id := b.nextSubID
	b.subscribers[id] = ch
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if sub, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(sub)
		}
		b.mu.Unlock()
	}
	return ch, unsubscribe
}

func (b *repoMetadataEventBus) publish(events ...RepoMetadataInvalidationEvent) {
	if len(events) == 0 {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.subscribers) == 0 {
		return
	}

	for _, event := range events {
		for _, ch := range b.subscribers {
			select {
			case ch <- event:
			default:
				log.Warn("Dropping repository metadata invalidation event due to slow consumer [repo_id: %d, version: %d]", event.RepoID, event.Version)
			}
		}
	}
}

// publishRepoMetadataInvalidation fans committed invalidation events out to
// subscribers. It must only be called after the underlying transaction has
// committed, otherwise a rolled back change would invalidate live caches.
func publishRepoMetadataInvalidation(events ...RepoMetadataInvalidationEvent) {
	repoMetadataEvents.publish(events...)
}

// RepoMetadataEventDeduper records the highest accepted event version per
// repository, letting consumers ignore duplicated or reordered events.
type RepoMetadataEventDeduper struct {
	mu       sync.Mutex
	versions map[int64]int64
}

// NewRepoMetadataEventDeduper returns an empty event deduper.
func NewRepoMetadataEventDeduper() *RepoMetadataEventDeduper {
	return &RepoMetadataEventDeduper{
		versions: make(map[int64]int64),
	}
}

// Accept reports whether the event carries a newer version than all previously
// accepted events of the same repository. Equal or lower versions, including
// duplicated deliveries, are rejected.
func (d *RepoMetadataEventDeduper) Accept(event RepoMetadataInvalidationEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if event.Version <= d.versions[event.RepoID] {
		return false
	}
	d.versions[event.RepoID] = event.Version
	return true
}
