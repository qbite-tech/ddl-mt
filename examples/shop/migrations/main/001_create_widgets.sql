-- Shared by development and production; ddl-mt verifies the selected database.
CREATE TABLE widgets (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
