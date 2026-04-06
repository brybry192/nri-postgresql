-- Standalone inventory database schema for e2e testing.
-- Separate from the primary/replica demo database to verify multi-instance monitoring.

CREATE TABLE IF NOT EXISTS warehouses (
    id       SERIAL PRIMARY KEY,
    name     VARCHAR(100) NOT NULL,
    location VARCHAR(100) NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
    id           SERIAL PRIMARY KEY,
    sku          VARCHAR(50)  NOT NULL UNIQUE,
    name         VARCHAR(255) NOT NULL,
    warehouse_id INTEGER      NOT NULL REFERENCES warehouses(id),
    quantity     INTEGER      NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_items_warehouse ON items(warehouse_id);
CREATE INDEX IF NOT EXISTS idx_items_sku       ON items(sku);

-- Seed warehouses.
INSERT INTO warehouses (name, location) VALUES
    ('East Coast', 'us-east-1'),
    ('West Coast', 'us-west-2'),
    ('Central',    'us-central-1')
ON CONFLICT DO NOTHING;

-- Seed 2000 items distributed across warehouses.
INSERT INTO items (sku, name, warehouse_id, quantity)
SELECT
    'INV-' || i,
    'Item ' || i,
    (i % 3) + 1,
    (random() * 500)::int
FROM generate_series(1, 2000) AS i
ON CONFLICT DO NOTHING;
