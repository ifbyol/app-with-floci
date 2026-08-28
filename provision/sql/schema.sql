CREATE TABLE IF NOT EXISTS movies (
    id         TEXT PRIMARY KEY,
    title      TEXT        NOT NULL,
    year       INTEGER     NOT NULL,
    genre      TEXT        NOT NULL,
    director   TEXT        NOT NULL,
    synopsis   TEXT        NOT NULL DEFAULT '',
    rating     NUMERIC(3,1) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS movies_genre_idx ON movies (genre);
