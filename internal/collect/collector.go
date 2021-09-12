package collect

import (
	"fmt"
	"io"
	"sync"

	"github.com/kaz/pprotein/internal/event"
)

type (
	Options struct {
		Type     string
		WorkDir  string
		FileName string
		EventHub *event.Hub
	}

	Collector struct {
		typeLabel string

		storage   *Storage
		processor Processor
		publisher *event.Publisher

		mu   *sync.RWMutex
		data map[string]*Entry
	}

	Entry struct {
		Snapshot *Snapshot
		Status   Status
		Message  string
	}
	Status string
)

const (
	StatusOk      Status = "ok"
	StatusFail    Status = "fail"
	StatusPending Status = "pending"
)

func New(processor Processor, opts *Options) (*Collector, error) {
	store, err := newStorage(opts.WorkDir, opts.FileName)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	c := &Collector{
		typeLabel: opts.Type,

		storage:   store,
		processor: newCachedProcessor(processor),
		publisher: opts.EventHub.Publisher(opts.Type),

		mu:   &sync.RWMutex{},
		data: map[string]*Entry{},
	}

	snapshots, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}

	for _, snapshot := range snapshots {
		go c.runProcessor(snapshot)
	}

	return c, nil
}

func (c *Collector) updateStatus(snapshot *Snapshot, status Status, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[snapshot.ID] = &Entry{
		Snapshot: snapshot,
		Status:   status,
		Message:  msg,
	}

	c.publisher.Publish()
}

func (c *Collector) runProcessor(snapshot *Snapshot) error {
	c.updateStatus(snapshot, StatusPending, "Processing")

	r, err := c.processor.Process(snapshot)
	if err != nil {
		go snapshot.Prune()
		c.updateStatus(snapshot, StatusFail, err.Error())
		return fmt.Errorf("processor aborted: %w", err)
	}
	if r != nil {
		r.Close()
	}

	c.updateStatus(snapshot, StatusOk, "Ready")
	return nil
}

func (c *Collector) Get(id string) (io.ReadCloser, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ent, ok := c.data[id]
	if !ok {
		return nil, fmt.Errorf("no such entry: %v", ent)
	}

	return c.processor.Process(ent.Snapshot)
}

func (c *Collector) List() []*Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resp := make([]*Entry, 0, len(c.data))
	for _, ent := range c.data {
		resp = append(resp, ent)
	}
	return resp
}

func (c *Collector) Collect(target *SnapshotTarget) error {
	if target.URL == "" || target.Duration == 0 {
		return fmt.Errorf("URL and Duration cannot be nil")
	}

	snapshot := c.storage.PrepareSnapshot(c.typeLabel, target)
	c.updateStatus(snapshot, StatusPending, "Collecting")

	if err := snapshot.Collect(); err != nil {
		c.updateStatus(snapshot, StatusFail, err.Error())
		return fmt.Errorf("failed to collect: %w", err)
	}

	if err := c.runProcessor(snapshot); err != nil {
		c.updateStatus(snapshot, StatusFail, err.Error())
		return fmt.Errorf("failed to process: %w", err)
	}
	return nil
}
