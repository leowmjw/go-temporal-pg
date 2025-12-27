-- Initialize source PostgreSQL database for testing
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Create test schemas
CREATE SCHEMA IF NOT EXISTS test_data;
CREATE SCHEMA IF NOT EXISTS app_data;

-- Create test tables with various data types
CREATE TABLE IF NOT EXISTS test_data.users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) UNIQUE NOT NULL,
    email VARCHAR(100) UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    is_active BOOLEAN DEFAULT true,
    metadata JSONB
);

CREATE TABLE IF NOT EXISTS test_data.orders (
    id SERIAL PRIMARY KEY,
    user_id INTEGER REFERENCES test_data.users(id),
    order_number VARCHAR(20) UNIQUE NOT NULL,
    total_amount DECIMAL(10,2) NOT NULL,
    status VARCHAR(20) DEFAULT 'pending',
    order_date DATE DEFAULT CURRENT_DATE,
    shipped_at TIMESTAMP WITH TIME ZONE,
    tracking_info TEXT
);

CREATE TABLE IF NOT EXISTS test_data.products (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    description TEXT,
    price DECIMAL(8,2) NOT NULL,
    category VARCHAR(50),
    stock_quantity INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS test_data.order_items (
    id SERIAL PRIMARY KEY,
    order_id INTEGER REFERENCES test_data.orders(id),
    product_id INTEGER REFERENCES test_data.products(id),
    quantity INTEGER NOT NULL,
    unit_price DECIMAL(8,2) NOT NULL,
    total_price DECIMAL(10,2) GENERATED ALWAYS AS (quantity * unit_price) STORED
);

-- Create indexes
CREATE INDEX idx_users_email ON test_data.users(email);
CREATE INDEX idx_users_created_at ON test_data.users(created_at);
CREATE INDEX idx_orders_user_id ON test_data.orders(user_id);
CREATE INDEX idx_orders_status ON test_data.orders(status);
CREATE INDEX idx_orders_date ON test_data.orders(order_date);
CREATE INDEX idx_products_category ON test_data.products(category);
CREATE INDEX idx_order_items_order_id ON test_data.order_items(order_id);
CREATE INDEX idx_order_items_product_id ON test_data.order_items(product_id);

-- Create sequences for testing
CREATE SEQUENCE IF NOT EXISTS test_data.custom_id_seq;

-- Create views
CREATE OR REPLACE VIEW test_data.order_summary AS
SELECT 
    o.id,
    o.order_number,
    u.username,
    u.email,
    o.total_amount,
    o.status,
    o.order_date,
    COUNT(oi.id) as item_count
FROM test_data.orders o
JOIN test_data.users u ON o.user_id = u.id
LEFT JOIN test_data.order_items oi ON o.id = oi.order_id
GROUP BY o.id, o.order_number, u.username, u.email, o.total_amount, o.status, o.order_date;

-- Create functions for testing
CREATE OR REPLACE FUNCTION test_data.update_user_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Create triggers
CREATE TRIGGER trigger_update_user_timestamp
    BEFORE UPDATE ON test_data.users
    FOR EACH ROW
    EXECUTE FUNCTION test_data.update_user_timestamp();

-- Insert sample data
INSERT INTO test_data.users (username, email, password_hash, metadata) VALUES
('admin', 'admin@test.com', crypt('admin123', gen_salt('bf')), '{"role": "admin", "preferences": {"theme": "dark"}}'),
('user1', 'user1@test.com', crypt('user123', gen_salt('bf')), '{"role": "user", "preferences": {"theme": "light"}}'),
('user2', 'user2@test.com', crypt('user123', gen_salt('bf')), '{"role": "user", "preferences": {"theme": "auto"}}'),
('user3', 'user3@test.com', crypt('user123', gen_salt('bf')), '{"role": "user", "preferences": {"theme": "light"}}');

INSERT INTO test_data.products (name, description, price, category, stock_quantity) VALUES
('Laptop', 'High-performance laptop', 999.99, 'Electronics', 50),
('Mouse', 'Wireless mouse', 29.99, 'Electronics', 100),
('Keyboard', 'Mechanical keyboard', 79.99, 'Electronics', 75),
('Monitor', '27-inch 4K monitor', 299.99, 'Electronics', 25),
('Desk Chair', 'Ergonomic office chair', 199.99, 'Furniture', 30);

INSERT INTO test_data.orders (user_id, order_number, total_amount, status) VALUES
(2, 'ORD-2024-001', 1029.98, 'completed'),
(3, 'ORD-2024-002', 109.98, 'shipped'),
(4, 'ORD-2024-003', 79.99, 'pending'),
(2, 'ORD-2024-004', 299.99, 'processing');

INSERT INTO test_data.order_items (order_id, product_id, quantity, unit_price) VALUES
(1, 1, 1, 999.99),
(1, 2, 1, 29.99),
(2, 2, 1, 29.99),
(2, 3, 1, 79.99),
(3, 3, 1, 79.99),
(4, 4, 1, 299.99);

-- Create replication publication for pgactive
CREATE PUBLICATION pgactive_pub FOR ALL TABLES;

-- Grant necessary permissions
GRANT USAGE ON SCHEMA test_data TO postgres;
GRANT ALL ON ALL TABLES IN SCHEMA test_data TO postgres;
GRANT ALL ON ALL SEQUENCES IN SCHEMA test_data TO postgres;

-- Create replication user for pgactive
CREATE USER pgactive_replicator WITH REPLICATION PASSWORD 'repl_pass_123';
GRANT USAGE ON SCHEMA test_data TO pgactive_replicator;
GRANT SELECT ON ALL TABLES IN SCHEMA test_data TO pgactive_replicator;

-- Log setup completion
INSERT INTO test_data.users (username, email, password_hash, metadata) VALUES
('test_init', 'init@test.com', 'setup_complete', '{"setup_timestamp": "' || NOW()::text || '"}');

SELECT 'Source database initialization completed' AS status;
