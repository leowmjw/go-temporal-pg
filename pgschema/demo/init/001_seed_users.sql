-- Seed schema for the pgschema demo: a small SaaS "users" table exactly as
-- it might exist in production BEFORE pgroll is adopted. This intentionally
-- does NOT go through pgroll — it's the brownfield starting point. pgroll
-- takes over from here via `pgroll init` (see mise task `demo-init`) and the
-- migrations in ../migrations/, run through the real pgschema Temporal
-- workflow.
CREATE TABLE users (
    id          SERIAL PRIMARY KEY,
    full_name   VARCHAR(255) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO users (full_name) VALUES
    ('Ada Lovelace'),
    ('Grace Hopper'),
    ('Alan Turing');
