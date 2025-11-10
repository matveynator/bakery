package inventory

import (
	"context"
	"errors"
	"time"
)

// command defines a mutation so the goroutine can serialize writes through a channel.
type command struct {
	action string
	item   Item
	id     int64
	reply  chan commandResult
}

// listQuery enables consumers to fetch the latest state without touching shared memory.
type listQuery struct {
	reply chan queryResult
}

// commandResult forwards either the persisted item or an error back to the caller.
type commandResult struct {
	item Item
	err  error
}

// queryResult returns a full list of inventory items for rendering.
type queryResult struct {
	items []Item
	err   error
}

// Service owns a goroutine to uphold Go's "share memory by communicating" approach.
type Service struct {
	repo      *Repository
	commands  chan command
	listCalls chan listQuery
	quit      chan struct{}
}

// NewService starts the background goroutine immediately so HTTP handlers only see non-blocking calls.
func NewService(repo *Repository) *Service {
	svc := &Service{
		repo:      repo,
		commands:  make(chan command),
		listCalls: make(chan listQuery),
		quit:      make(chan struct{}),
	}
	go svc.loop()
	return svc
}

// loop processes commands and queries sequentially so no mutexes are needed.
func (s *Service) loop() {
	for {
		select {
		case cmd := <-s.commands:
			switch cmd.action {
			case "save":
				stored, err := s.repo.Save(context.Background(), cmd.item)
				cmd.reply <- commandResult{item: stored, err: err}
			case "update":
				err := s.repo.Update(context.Background(), cmd.item)
				cmd.reply <- commandResult{err: err}
			case "delete":
				err := s.repo.Delete(context.Background(), cmd.id)
				cmd.reply <- commandResult{err: err}
			default:
				cmd.reply <- commandResult{err: errors.New("unknown inventory action")}
			}
		case q := <-s.listCalls:
			items, err := s.repo.List(context.Background())
			q.reply <- queryResult{items: items, err: err}
		case <-s.quit:
			return
		}
	}
}

// Add registers a fresh batch and returns the stored record with its generated identifier.
func (s *Service) Add(ctx context.Context, item Item) (Item, error) {
	reply := make(chan commandResult)
	cmd := command{action: "save", item: item, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return Item{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Item{}, errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.item, res.err
	case <-ctx.Done():
		return Item{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Item{}, errors.New("inventory save timed out")
	}
}

// Update mutates the available count or price when the admin edits a row.
func (s *Service) Update(ctx context.Context, item Item) error {
	reply := make(chan commandResult)
	cmd := command{action: "update", item: item, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory update timed out")
	}
}

// Delete removes the batch entirely when the admin clears it.
func (s *Service) Delete(ctx context.Context, id int64) error {
	reply := make(chan commandResult)
	cmd := command{action: "delete", id: id, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory delete timed out")
	}
}

// List returns all batches to render the admin table and the public menu.
func (s *Service) List(ctx context.Context) ([]Item, error) {
	reply := make(chan queryResult)
	q := listQuery{reply: reply}

	select {
	case s.listCalls <- q:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.items, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("inventory list timed out")
	}
}

// Close stops the background goroutine when the application shuts down.
func (s *Service) Close() {
	close(s.quit)
}
