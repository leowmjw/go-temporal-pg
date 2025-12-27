-- Initialize target PostgreSQL database for testing
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Create test schemas (will be populated by replication)
CREATE SCHEMA IF NOT EXISTS test_data;
CREATE SCHEMA IF NOT EXISTS app_data;

-- Create replication subscription slot (will be managed by pgactive)
-- This is just a placeholder - pgactive will manage the actual replication

-- Grant necessary permissions for replication
GRANT USAGE ON SCHEMA test_data TO postgres;
GRANT ALL ON ALL TABLES IN SCHEMA test_data TO postgres;
GRANT ALL ON ALL SEQUENCES IN SCHEMA test_data TO postgres;

-- Create replication user for pgactive
CREATE USER pgactive_replicator WITH REPLICATION PASSWORD 'repl_pass_123';
GRANT USAGE ON SCHEMA test_data TO pgactive_replicator;
GRANT ALL ON ALL TABLES IN SCHEMA test_data TO pgactive_replicator;
GRANT ALL ON ALL SEQUENCES IN SCHEMA test_data TO pgactive_replicator;

-- Log setup completion
CREATE TABLE IF NOT EXISTS test_data.target_init_log (
    id SERIAL PRIMARY KEY,
    message TEXT,
    timestamp TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

INSERT INTO test_data.target_init_log (message) VALUES 
('Target database initialization completed');

SELECT 'Target database initialization completed' AS status;
