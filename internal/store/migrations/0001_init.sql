CREATE TABLE flags (
    name               TEXT PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    description        TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    enabled            BOOLEAN NOT NULL DEFAULT FALSE,
    rollout_percentage SMALLINT NOT NULL DEFAULT 100 CHECK (rollout_percentage BETWEEN 0 AND 100),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE flag_overrides (
    flag_name  TEXT NOT NULL REFERENCES flags (name) ON DELETE CASCADE,
    user_id    TEXT NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 128),
    enabled    BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (flag_name, user_id)
);

-- A flag that other flags require cannot be deleted (ON DELETE RESTRICT).
CREATE TABLE flag_prerequisites (
    flag_name     TEXT NOT NULL REFERENCES flags (name) ON DELETE CASCADE,
    requires_flag TEXT NOT NULL REFERENCES flags (name) ON DELETE RESTRICT,
    PRIMARY KEY (flag_name, requires_flag),
    CHECK (flag_name <> requires_flag)
);

-- Finds the flags that depend on a given flag.
CREATE INDEX flag_prerequisites_requires_flag_idx ON flag_prerequisites (requires_flag);
