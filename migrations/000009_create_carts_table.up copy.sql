BEGIN;
CREATE TABLE IF NOT EXISTS carts (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    quantity INTEGER NOT NULL DEFAULT 1,
    -- THIS IS THE FIX: It prevents two rows for the same user/item pair
    CONSTRAINT unique_user_item UNIQUE (user_id, item_id)
);
CREATE INDEX idx_carts_user_id ON carts (user_id);
COMMIT;