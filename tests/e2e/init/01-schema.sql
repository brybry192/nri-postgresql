-- E2E test schema: creates realistic tables and indexes so the integration
-- collects richer metrics (table sizes, index usage, etc.) than an empty DB.

CREATE TABLE IF NOT EXISTS orders (
    id          SERIAL PRIMARY KEY,
    customer_id INTEGER      NOT NULL,
    status      VARCHAR(20)  NOT NULL DEFAULT 'pending',
    total_cents INTEGER      NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS customers (
    id         SERIAL PRIMARY KEY,
    email      VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS products (
    id          SERIAL PRIMARY KEY,
    sku         VARCHAR(50)  NOT NULL UNIQUE,
    name        VARCHAR(255) NOT NULL,
    price_cents INTEGER      NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_orders_customer  ON orders(customer_id);
CREATE INDEX IF NOT EXISTS idx_orders_status    ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_created   ON orders(created_at);

-- Seed with enough rows that the integration reports non-trivial sizes.
INSERT INTO customers (email)
SELECT 'user' || i || '@example.com'
FROM generate_series(1, 500) AS i
ON CONFLICT DO NOTHING;

INSERT INTO products (sku, name, price_cents)
SELECT 'SKU-' || i, 'Product ' || i, (random() * 10000)::int
FROM generate_series(1, 100) AS i
ON CONFLICT DO NOTHING;

INSERT INTO orders (customer_id, status, total_cents)
SELECT
    (random() * 499 + 1)::int,
    (ARRAY['pending','paid','shipped','cancelled'])[floor(random() * 4 + 1)],
    (random() * 50000 + 100)::int
FROM generate_series(1, 5000);
