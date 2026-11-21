-- One row per change to a flag: who changed it, when, and the value before
-- and after. Rows are only ever inserted.
--
-- flag_name deliberately has no foreign key to flags, so that the history of
-- a flag survives the flag being deleted.
CREATE TABLE audit_log (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    flag_name      TEXT NOT NULL,
    action         TEXT NOT NULL,
    actor          TEXT NOT NULL,
    previous_value JSONB,
    new_value      JSONB,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves "history of one flag, newest first".
CREATE INDEX audit_log_flag_idx ON audit_log (flag_name, id DESC);
