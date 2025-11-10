package memorydriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// orderRecord keeps the raw persisted representation for the lightweight driver.
type orderRecord struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Address       string    `json:"address"`
	Phone         string    `json:"phone"`
	ItemsJSON     string    `json:"items"`
	BreadJSON     string    `json:"bread_schedule"`
	CroissantJSON string    `json:"croissant_schedule"`
	Comment       string    `json:"comment"`
	CreatedAt     time.Time `json:"created_at"`
}

// inventoryRecord tracks available batches so the admin panel can read and mutate them.
type inventoryRecord struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Category       string    `json:"category"`
	AvailableCount int       `json:"available_count"`
	PriceCents     int       `json:"price_cents"`
	BakedAt        time.Time `json:"baked_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// snapshot is written to disk after each mutation so the driver survives restarts.
type snapshot struct {
	Orders           []orderRecord     `json:"orders"`
	Inventory        []inventoryRecord `json:"inventory"`
	OrderCounter     int64             `json:"order_counter"`
	InventoryCounter int64             `json:"inventory_counter"`
}

// storeCommand models every operation executed against the in-memory store.
type storeCommand struct {
	action    string
	order     orderRecord
	inventory inventoryRecord
	id        int64
	reply     chan storeResult
}

// storeResult transfers either the new identifier, a record list, or an error.
type storeResult struct {
	id        int64
	orders    []orderRecord
	inventory []inventoryRecord
	err       error
}

// store keeps the sequence of orders guarded by a dedicated goroutine.
type store struct {
	commands         chan storeCommand
	closed           chan struct{}
	persistRequests  chan snapshot
	orders           []orderRecord
	inventory        []inventoryRecord
	orderCounter     int64
	inventoryCounter int64
	snapshotPath     string
}

// newStore creates a store and spins the goroutines so every access flows through a channel.
func newStore(path string) (*store, error) {
	loaded, err := readSnapshot(path)
	if err != nil {
		return nil, err
	}
	s := &store{
		// A small buffer keeps bootstrap operations from blocking before the store goroutine spins up.
		commands:        make(chan storeCommand, 32),
		closed:          make(chan struct{}),
		persistRequests: make(chan snapshot, 1),
		snapshotPath:    path,
	}
	if loaded != nil {
		s.orders = loaded.Orders
		s.inventory = loaded.Inventory
		s.orderCounter = loaded.OrderCounter
		s.inventoryCounter = loaded.InventoryCounter
	}
	go s.loop()
	go s.persistenceLoop()
	return s, nil
}

// loop serializes every mutation and read request to keep the state safe without mutexes.
func (s *store) loop() {
	for {
		select {
		case cmd := <-s.commands:
			switch cmd.action {
			case "insertOrder":
				id := atomic.AddInt64(&s.orderCounter, 1)
				cmd.order.ID = id
				cmd.order.CreatedAt = time.Now().UTC()
				s.orders = append(s.orders, cmd.order)
				s.queuePersist()
				cmd.reply <- storeResult{id: id}
			case "listOrders":
				cloned := cloneOrders(s.orders)
				cmd.reply <- storeResult{orders: cloned}
			case "insertInventory":
				id := atomic.AddInt64(&s.inventoryCounter, 1)
				cmd.inventory.ID = id
				cmd.inventory.CreatedAt = time.Now().UTC()
				s.inventory = append(s.inventory, cmd.inventory)
				s.queuePersist()
				cmd.reply <- storeResult{id: id}
			case "listInventory":
				cmd.reply <- storeResult{inventory: cloneInventory(s.inventory)}
			case "updateInventory":
				updated := false
				for i := range s.inventory {
					if s.inventory[i].ID == cmd.inventory.ID {
						if cmd.inventory.AvailableCount >= 0 {
							s.inventory[i].AvailableCount = cmd.inventory.AvailableCount
						}
						if cmd.inventory.PriceCents >= 0 {
							s.inventory[i].PriceCents = cmd.inventory.PriceCents
						}
						if !cmd.inventory.BakedAt.IsZero() {
							s.inventory[i].BakedAt = cmd.inventory.BakedAt
						}
						updated = true
						break
					}
				}
				if !updated {
					cmd.reply <- storeResult{err: errors.New("inventory item not found")}
					continue
				}
				s.queuePersist()
				cmd.reply <- storeResult{}
			case "deleteInventory":
				removed := false
				for i := range s.inventory {
					if s.inventory[i].ID == cmd.id {
						s.inventory = append(s.inventory[:i], s.inventory[i+1:]...)
						removed = true
						break
					}
				}
				if !removed {
					cmd.reply <- storeResult{err: errors.New("inventory item not found")}
					continue
				}
				s.queuePersist()
				cmd.reply <- storeResult{}
			case "noop":
				cmd.reply <- storeResult{}
			default:
				cmd.reply <- storeResult{err: fmt.Errorf("unsupported action %s", cmd.action)}
			}
		case <-s.closed:
			return
		}
	}
}

// persistenceLoop writes snapshots asynchronously so the main loop stays responsive.
func (s *store) persistenceLoop() {
	for {
		select {
		case snap := <-s.persistRequests:
			if s.snapshotPath == "" {
				continue
			}
			_ = writeSnapshot(s.snapshotPath, snap)
		case <-s.closed:
			return
		}
	}
}

// queuePersist sends the current snapshot to the background writer without blocking.
func (s *store) queuePersist() {
	if s.snapshotPath == "" {
		return
	}
	snap := snapshot{
		Orders:           cloneOrders(s.orders),
		Inventory:        cloneInventory(s.inventory),
		OrderCounter:     atomic.LoadInt64(&s.orderCounter),
		InventoryCounter: atomic.LoadInt64(&s.inventoryCounter),
	}
	select {
	case s.persistRequests <- snap:
	default:
		<-s.persistRequests
		s.persistRequests <- snap
	}
}

// close stops the goroutine; the server keeps it alive for the entire process lifetime.
func (s *store) close() {
	close(s.closed)
}

// Driver wires the store into the database/sql world.
type Driver struct {
	store *store
}

// Open creates a connection that forwards calls to the shared store.
func (d *Driver) Open(name string) (driver.Conn, error) {
	if d.store == nil {
		return nil, errors.New("memory driver store is not initialized")
	}
	return &conn{store: d.store}, nil
}

// conn represents a lightweight connection object; every operation still travels through channels.
type conn struct {
	store *store
}

// Prepare builds a statement object for the small set of supported queries.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	trimmed := strings.TrimSpace(strings.ToLower(query))
	switch {
	case strings.HasPrefix(trimmed, "insert into orders"):
		return &stmt{store: c.store, query: "insertOrder"}, nil
	case strings.HasPrefix(trimmed, "select") && strings.Contains(trimmed, "from orders"):
		return &stmt{store: c.store, query: "listOrders"}, nil
	case strings.HasPrefix(trimmed, "insert into inventory"):
		return &stmt{store: c.store, query: "insertInventory"}, nil
	case strings.HasPrefix(trimmed, "select") && strings.Contains(trimmed, "from inventory"):
		return &stmt{store: c.store, query: "listInventory"}, nil
	case strings.HasPrefix(trimmed, "update inventory"):
		return &stmt{store: c.store, query: "updateInventory"}, nil
	case strings.HasPrefix(trimmed, "delete from inventory"):
		return &stmt{store: c.store, query: "deleteInventory"}, nil
	case strings.HasPrefix(trimmed, "create table"):
		return &stmt{store: c.store, query: "noop"}, nil
	default:
		return nil, fmt.Errorf("unsupported query: %s", query)
	}
}

// Close is a no-op because the shared store owns the lifecycle.
func (c *conn) Close() error { return nil }

// Begin is not implemented because the project operates without transactions.
func (c *conn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported by the memory driver")
}

// stmt forwards Exec and Query to the store with the data shaped for each case.
type stmt struct {
	store *store
	query string
}

// Close is a no-op since statements do not maintain resources in this simple driver.
func (s *stmt) Close() error { return nil }

// NumInput matches the driver.Stmt contract; -1 allows database/sql to accept any argument count.
func (s *stmt) NumInput() int { return -1 }

// Exec handles the mutation statements supported by the driver.
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	if s.query == "noop" {
		// Schema bootstrap statements do not touch the in-memory store, so we short-circuit them.
		return execResult{}, nil
	}
	reply := make(chan storeResult)
	cmd := storeCommand{action: s.query, reply: reply}

	switch s.query {
	case "insertOrder":
		if len(args) < 7 {
			return nil, fmt.Errorf("expected 7 arguments, got %d", len(args))
		}
		cmd.order = orderRecord{
			Name:          toString(args[0]),
			Address:       toString(args[1]),
			Phone:         toString(args[2]),
			ItemsJSON:     toString(args[3]),
			BreadJSON:     toString(args[4]),
			CroissantJSON: toString(args[5]),
			Comment:       toString(args[6]),
		}
	case "insertInventory":
		if len(args) < 5 {
			return nil, fmt.Errorf("expected 5 arguments, got %d", len(args))
		}
		baked, err := toTime(args[4])
		if err != nil {
			return nil, err
		}
		cmd.inventory = inventoryRecord{
			Name:           toString(args[0]),
			Category:       toString(args[1]),
			AvailableCount: toInt(args[2]),
			PriceCents:     toInt(args[3]),
			BakedAt:        baked,
		}
	case "updateInventory":
		if len(args) < 4 {
			return nil, fmt.Errorf("expected 4 arguments, got %d", len(args))
		}
		baked, err := toTime(args[2])
		if err != nil {
			return nil, err
		}
		cmd.inventory = inventoryRecord{
			AvailableCount: toInt(args[0]),
			PriceCents:     toInt(args[1]),
			BakedAt:        baked,
			ID:             toInt64(args[3]),
		}
	case "deleteInventory":
		if len(args) < 1 {
			return nil, errors.New("expected id for delete")
		}
		cmd.id = toInt64(args[0])
	default:
		return nil, fmt.Errorf("unsupported exec action %s", s.query)
	}

	if err := s.enqueue(cmd); err != nil {
		return nil, err
	}

	res := <-reply
	if res.err != nil {
		return nil, res.err
	}
	return execResult{id: res.id}, nil
}

// enqueue sends the command to the store while honoring a timeout to avoid blocking forever.
func (s *stmt) enqueue(cmd storeCommand) error {
	select {
	case s.store.commands <- cmd:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("timed out while enqueuing command")
	}
}

// Query fetches the stored records and converts them into driver.Rows.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	reply := make(chan storeResult)
	cmd := storeCommand{action: s.query, reply: reply}

	if err := s.enqueue(cmd); err != nil {
		return nil, err
	}

	res := <-reply
	if res.err != nil {
		return nil, res.err
	}
	switch s.query {
	case "listOrders":
		return &rows{kind: "orders", orders: res.orders}, nil
	case "listInventory":
		return &rows{kind: "inventory", inventory: res.inventory}, nil
	default:
		return nil, errors.New("query only supports listing")
	}
}

// execResult fulfills the driver.Result interface with the generated identifier.
type execResult struct {
	id int64
}

func (r execResult) LastInsertId() (int64, error) { return r.id, nil }
func (r execResult) RowsAffected() (int64, error) {
	if r.id == 0 {
		return 0, nil
	}
	return 1, nil
}

// rows iterates through the stored records while serving Columns and Next calls.
type rows struct {
	kind      string
	orders    []orderRecord
	inventory []inventoryRecord
	index     int
}

// Columns aligns with the SELECT projection used by the repository.
func (r *rows) Columns() []string {
	if r.kind == "inventory" {
		return []string{"id", "name", "category", "available_count", "price_cents", "baked_at"}
	}
	return []string{"id", "name", "address", "phone", "items", "bread_schedule", "croissant_schedule", "comment"}
}

// Close is a no-op for the lightweight row iterator.
func (r *rows) Close() error { return nil }

// Next moves through the records and writes the column data into the provided slice.
func (r *rows) Next(dest []driver.Value) error {
	switch r.kind {
	case "inventory":
		if r.index >= len(r.inventory) {
			return io.EOF
		}
		record := r.inventory[r.index]
		r.index++
		dest[0] = record.ID
		dest[1] = record.Name
		dest[2] = record.Category
		dest[3] = record.AvailableCount
		dest[4] = record.PriceCents
		dest[5] = record.BakedAt
		return nil
	default:
		if r.index >= len(r.orders) {
			return io.EOF
		}
		record := r.orders[r.index]
		r.index++
		dest[0] = record.ID
		dest[1] = record.Name
		dest[2] = record.Address
		dest[3] = record.Phone
		dest[4] = record.ItemsJSON
		dest[5] = record.BreadJSON
		dest[6] = record.CroissantJSON
		dest[7] = record.Comment
		return nil
	}
}

// toString converts driver.Value into a usable string.
func toString(value driver.Value) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// toInt converts driver.Value to an int for inventory counters.
func toInt(value driver.Value) int {
	switch v := value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	case float32:
		return int(v)
	case string:
		if v == "" {
			return 0
		}
		var parsed int
		fmt.Sscanf(v, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

// toInt64 converts driver.Value to int64 when IDs are involved.
func toInt64(value driver.Value) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(math.Round(v))
	case string:
		if v == "" {
			return 0
		}
		var parsed int64
		fmt.Sscanf(v, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

// toTime handles the baked_at column conversions.
func toTime(value driver.Value) (time.Time, error) {
	switch v := value.(type) {
	case time.Time:
		return v.UTC(), nil
	case string:
		if v == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, err
		}
		return parsed.UTC(), nil
	case []byte:
		if len(v) == 0 {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, string(v))
		if err != nil {
			return time.Time{}, err
		}
		return parsed.UTC(), nil
	default:
		return time.Time{}, errors.New("unsupported time format")
	}
}

// Register exposes the driver under the requested label for consumers.
func Register(dbType, path string) (string, func(), error) {
	switch dbType {
	case "chai", "sqlite", "duckdb", "pgx", "clickhouse":
	default:
		return "", func() {}, fmt.Errorf("unsupported db type %s", dbType)
	}
	driverName := "bakery-" + dbType
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", func() {}, err
		}
		path = filepath.Join(cwd, driverName+".json")
	}
	store, err := newStore(path)
	if err != nil {
		return "", func() {}, err
	}
	sql.Register(driverName, &Driver{store: store})
	cleanup := func() {
		store.queuePersist()
		store.close()
	}
	return driverName, cleanup, nil
}

// EnsureSchema executes CREATE TABLE statements so external databases get the right layout.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS orders (
                        id INTEGER PRIMARY KEY,
                        name TEXT,
                        address TEXT,
                        phone TEXT,
                        items TEXT,
                        bread_schedule TEXT,
                        croissant_schedule TEXT,
                        comment TEXT
                )`,
		`CREATE TABLE IF NOT EXISTS inventory (
                        id INTEGER PRIMARY KEY,
                        name TEXT,
                        category TEXT,
                        available_count INTEGER,
                        price_cents INTEGER,
                        baked_at TIMESTAMP
                )`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unsupported") {
				continue
			}
			return err
		}
	}
	return nil
}

// readSnapshot loads the persisted JSON file if it exists.
func readSnapshot(path string) (*snapshot, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// writeSnapshot persists the current state to disk.
func writeSnapshot(path string, snap snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// cloneOrders duplicates the slice so callers cannot mutate internal state.
func cloneOrders(src []orderRecord) []orderRecord {
	out := make([]orderRecord, len(src))
	copy(out, src)
	return out
}

// cloneInventory duplicates the inventory slice for safe sharing.
func cloneInventory(src []inventoryRecord) []inventoryRecord {
	out := make([]inventoryRecord, len(src))
	copy(out, src)
	return out
}
