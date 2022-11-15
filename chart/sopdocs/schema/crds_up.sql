-- Create database if not exists
SELECT 'CREATE DATABASE doc'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'doc')\gexec

\connect doc;



CREATE TABLE IF NOT EXISTS tags (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    repo VARCHAR(255) NOT NULL,
    time TIMESTAMP NOT NULL,
    UNIQUE(name, repo)
);

CREATE TABLE IF NOT EXISTS crds (
    "group" VARCHAR(255) NOT NULL,
    version VARCHAR(255) NOT NULL,
    kind VARCHAR(255) NOT NULL,
    tag_id INTEGER NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    filename VARCHAR(255) NOT NULL,
    data JSONB NOT NULL,
    PRIMARY KEY(tag_id, "group", version, kind)
);
