package store

import (
	"checkout-api/models"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is an in-memory store for items and orders.
type PostgresStore struct {
	conn *pgxpool.Pool
}

func NewPostgresStore(conn *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{
		conn: conn,
	}
}

func (s *PostgresStore) DB() *Query {
	return &Query{
		DBTX: s.conn,
	}
}

func (s *PostgresStore) WithTx(tx pgx.Tx) *Query {
	return &Query{
		DBTX: tx,
	}
}

func (s *PostgresStore) GetItems(ctx context.Context) ([]*models.Item, error) {
	rows, err := s.DB().GetItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to select all items", err)
	}
	defer rows.Close()

	items := make([]*models.Item, 0)
	for rows.Next() {
		var item models.Item
		err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Price, &item.Stock, &item.CreatedAt)
		if err != nil {
			fmt.Printf("unable to scan row: %v\n", err)
			return nil, fmt.Errorf("unable to scan row: %w", err)
		}
		items = append(items, &item)
	}
	if rows.Err() != nil {
		return nil, err
	}

	return items, nil
}

func (s *PostgresStore) GetItem(ctx context.Context, id int) (*models.Item, error) {
	var item models.Item
	err := s.DB().GetItemByID(ctx, id).Scan(&item.ID, &item.Name, &item.Description, &item.Price, &item.Stock, &item.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *PostgresStore) CreateOrder(ctx context.Context, userID int, items []models.LineItem, total int, status string) (*models.Order, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	q := s.WithTx(tx)

	// Pessimistic lock: acquire row-level lock on each item and verify sufficient stock.
	for _, lineItem := range items {
		var item models.Item
		err := q.GetItemByIDForUpdate(ctx, lineItem.ItemID).Scan(&item.ID, &item.Name, &item.Description, &item.Price, &item.Stock, &item.CreatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("item %d not found", lineItem.ItemID)
			}
			return nil, fmt.Errorf("failed to lock item %d: %w", lineItem.ItemID, err)
		}
		if item.Stock < lineItem.Quantity {
			return nil, fmt.Errorf("insufficient stock for item %d: have %d, need %d", lineItem.ItemID, item.Stock, lineItem.Quantity)
		}
	}

	var orderID int
	if err := q.InsertOrderReturning(ctx, userID, total, status).Scan(&orderID); err != nil {
		return nil, fmt.Errorf("failed to insert order: %w", err)
	}

	for _, lineItem := range items {
		if _, err := q.InsertLineItem(ctx, orderID, lineItem.ItemID, lineItem.Price, lineItem.Quantity); err != nil {
			return nil, fmt.Errorf("failed to insert line item for item %d: %w", lineItem.ItemID, err)
		}
		if _, err := q.DecrementItemStock(ctx, lineItem.ItemID, lineItem.Quantity); err != nil {
			return nil, fmt.Errorf("failed to decrement stock for item %d: %w", lineItem.ItemID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return &models.Order{
		ID:     orderID,
		UserID: userID,
		Items:  items,
		Total:  total,
		Status: status,
	}, nil
}

func (s *PostgresStore) UpdateOrderStatus(ctx context.Context, orderID int, status string) error {
	_, err := s.DB().UpdateOrderStatus(ctx, orderID, status)
	return err
}

func (s *PostgresStore) UpsertCartItem(ctx context.Context, userID int, itemID int, quantity int) error {
	_, err := s.DB().UpsertCart(ctx, userID, itemID, quantity)
	return err
}

func (s *PostgresStore) GetUserCart(ctx context.Context, userID int) ([]models.CartItemResponse, error) {

	// TODO: returning a slice of models.Cart does not seem useful for our API
	// return a slice of items that belong to the user instead
	// use GetItemsFromUserCart(ctx context.Context, userID int) (pgx.Rows, error)
	query := `
	SELECT i.id, i.name, i.description, i.price, i.stock, i.created_at, c.quantity
	FROM carts c
	JOIN items i ON i.id = c.item_id
	WHERE c.user_id = $1
	`

	rows, err := s.conn.Query(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cartItems []models.CartItemResponse
	for rows.Next() {
		var item models.CartItemResponse
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Price, &item.Stock, &item.CreatedAt, &item.Quantity); err != nil {
			return nil, err
		}
		cartItems = append(cartItems, item)
	}

	return cartItems, nil
}

func (s *PostgresStore) DeleteUserCart(ctx context.Context, userID int) error {
	_, err := s.DB().DeleteCartByUserID(ctx, userID)
	return err
}

func (s *PostgresStore) RemoveCartItem(ctx context.Context, userID int, itemID int) error {
	_, err := s.DB().DeleteItemFromUserCart(ctx, userID, itemID)
	return err
}

func (s *PostgresStore) SaveUser(ctx context.Context, email string, hash []byte) error {
	// Added public.
	_, err := s.conn.Exec(ctx,
		`INSERT INTO public.users (email, hash, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`,
		email, hash,
	)
	return err
}

func (s *PostgresStore) FindUserByEmail(ctx context.Context, email string) (models.User, error) {
	var user models.User
	row := s.conn.QueryRow(ctx, `SELECT id, email, hash FROM users WHERE email = $1`, email)
	err := row.Scan(&user.ID, &user.Email, &user.Hash) // user.Hash must be []byte
	if err != nil {
		return models.User{}, err
	}
	return user, nil
}

func (s *PostgresStore) FindUserByID(ctx context.Context, id int) (models.User, error) {
	var user models.User
	row := s.conn.QueryRow(ctx, `SELECT id, email, hash FROM users WHERE id = $1`, id)
	err := row.Scan(&user.ID, &user.Email, &user.Hash) // user.Hash must be []byte
	if err != nil {
		return models.User{}, err
	}
	return user, nil
}

func (s *PostgresStore) ValidateRefreshToken(ctx context.Context, token string) (int, error) {
	var userID int
	query := `SELECT user_id FROM public.refresh_tokens WHERE token = $1 AND active = true`
	err := s.conn.QueryRow(ctx, query, token).Scan(&userID)
	if err != nil {
		return 0, err
	}
	return userID, nil
}

func (s *PostgresStore) SaveRefreshToken(ctx context.Context, userID int, token string) error {
	query := `INSERT INTO public.refresh_tokens (user_id, token, active) VALUES ($1, $2, true)`
	_, err := s.conn.Exec(ctx, query, userID, token)
	return err
}

func (s *PostgresStore) DeactivateRefreshToken(ctx context.Context, token string) error {
	query := `UPDATE public.refresh_tokens SET active = false WHERE token = $1`
	_, err := s.conn.Exec(ctx, query, token)
	return err
}

func (s *PostgresStore) SaveIdempotencyKey(ctx context.Context, id string, status_code int, expiry time.Time, response []byte) error {
	_, err := s.conn.Exec(ctx, "insert into idempotency_keys (id, status_code, expiry, response) values ($1, $2, $3, $4)", id, status_code, expiry, response)
	return err
}

func (s *PostgresStore) FindIdempotencyKey(ctx context.Context, id string) (*models.IdempotencyRecord, error) {
	var record models.IdempotencyRecord
	row := s.conn.QueryRow(ctx, `SELECT id, response, status_code, expiry FROM idempotency_keys WHERE id = $1`, id)
	err := row.Scan(&record.ID, &record.Response, &record.StatusCode, &record.Expiry)
	if err != nil {
		return &models.IdempotencyRecord{}, err
	}
	return &record, nil
}

func (s *PostgresStore) DeleteIdempotencyKey(ctx context.Context, id string) error {
	_, err := s.conn.Exec(ctx, "delete from idempotency_keys where id = $1", id)
	return err
}
