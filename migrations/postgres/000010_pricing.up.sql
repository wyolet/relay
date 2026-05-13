-- 000010: pricing entity (owned by Host, targets ≥1 Models).
--
-- Single rate sheet per row. Rates ([]Rate) and Currency live in the JSONB
-- spec; the target-model set lives in the pricing_models junction so we can
-- index reachability without parsing JSONB.
--
-- host_id is a column (not just JSONB-embedded) so FK cascade handles host
-- deletes and the snapshot builder can join without JSONB parsing.

CREATE TABLE IF NOT EXISTS pricings (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    host_id      TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS pricings_host_idx ON pricings (host_id);

CREATE TABLE IF NOT EXISTS pricing_models (
    pricing_id TEXT NOT NULL REFERENCES pricings(id) ON DELETE CASCADE,
    model_id   TEXT NOT NULL REFERENCES models(id)   ON DELETE RESTRICT,
    position   INT  NOT NULL,
    PRIMARY KEY (pricing_id, model_id)
);
CREATE INDEX IF NOT EXISTS pricing_models_model_idx ON pricing_models (model_id);
