package order

import (
	"context"
	"errors"
	"strings"
	"time"
)

// validationError communicates rule violations back to HTTP handlers.
type validationError struct {
	message string
}

func (e validationError) Error() string { return e.message }

// newValidationError keeps the constructor private to the package.
func newValidationError(msg string) error {
	return validationError{message: msg}
}

// IsValidation helps callers distinguish between business and infrastructure failures.
func IsValidation(err error) bool {
	var v validationError
	return errors.As(err, &v)
}

// command envelopes the work the service goroutine must perform.
type command struct {
	order Order
	reply chan commandResult
}

// query allows different consumers to request the current order list.
type query struct {
	reply chan queryResult
}

// commandResult contains the stored order or an error to propagate back to the caller.
type commandResult struct {
	order Order
	err   error
}

// queryResult contains the aggregated orders alongside potential failures.
type queryResult struct {
	orders []Order
	err    error
}

// Service orchestrates the asynchronous handling of incoming orders.
type Service struct {
	repo          *Repository
	commands      chan command
	queries       chan query
	cancellations chan struct{}
}

// NewService launches the coordinating goroutine immediately so requests never block the caller for scheduling.
func NewService(repo *Repository) *Service {
	svc := &Service{
		repo:          repo,
		commands:      make(chan command),
		queries:       make(chan query),
		cancellations: make(chan struct{}),
	}
	go svc.loop()
	return svc
}

// loop listens to commands and queries so the service honors the Go proverb "Don't communicate by sharing memory".
func (s *Service) loop() {
	for {
		select {
		case cmd := <-s.commands:
			if err := validateOrder(cmd.order); err != nil {
				cmd.reply <- commandResult{err: err}
				continue
			}
			stored, err := s.repo.Save(context.Background(), cmd.order)
			cmd.reply <- commandResult{order: stored, err: err}
		case q := <-s.queries:
			orders, err := s.repo.List(context.Background())
			q.reply <- queryResult{orders: orders, err: err}
		case <-s.cancellations:
			return
		}
	}
}

// Submit registers a new order request and waits for the background goroutine to persist it.
func (s *Service) Submit(ctx context.Context, order Order) (Order, error) {
	reply := make(chan commandResult)
	cmd := command{order: order, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return Order{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Order{}, errors.New("queue is busy processing other orders")
	}

	select {
	case res := <-reply:
		return res.order, res.err
	case <-ctx.Done():
		return Order{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Order{}, errors.New("order processing took too long")
	}
}

// List returns the stored orders; useful for dashboards or tests.
func (s *Service) List(ctx context.Context) ([]Order, error) {
	reply := make(chan queryResult)
	req := query{reply: reply}

	select {
	case s.queries <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("queue is busy processing other orders")
	}

	select {
	case res := <-reply:
		return res.orders, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("listing orders took too long")
	}
}

// Close stops the goroutine to allow graceful shutdown.
func (s *Service) Close() {
	close(s.cancellations)
}

// validateOrder keeps the business rules near the service so multiple HTTP endpoints can reuse them.
func validateOrder(order Order) error {
	if strings.TrimSpace(order.CustomerName) == "" {
		return newValidationError("name is required")
	}
	if strings.TrimSpace(order.Address) == "" {
		return newValidationError("address is required")
	}
	if strings.TrimSpace(order.Phone) == "" {
		return newValidationError("phone is required")
	}
	if len(order.Items) == 0 {
		return newValidationError("at least one item is required")
	}
	if len(order.BreadSchedule.Days) == 0 {
		return newValidationError("select at least one bread delivery day")
	}
	if order.BreadSchedule.Frequency == "" {
		return newValidationError("select a bread delivery frequency")
	}
	if strings.TrimSpace(order.BreadSchedule.StartDate) == "" {
		return newValidationError("select a bread start date")
	}
	if len(order.CroissantSchedule) == 0 {
		return newValidationError("select croissant days")
	}
	for _, slot := range order.CroissantSchedule {
		if slot.Day == "" {
			return newValidationError("croissant day is required")
		}
		if slot.Quantity <= 0 {
			return newValidationError("croissant quantity must be positive")
		}
	}
	return nil
}
