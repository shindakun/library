package catalog

const schema = `
CREATE TABLE IF NOT EXISTS books (
  id          INTEGER PRIMARY KEY,
  title       TEXT NOT NULL,
  sort_title  TEXT,
  path        TEXT NOT NULL UNIQUE,
  file_size   INTEGER,
  file_hash   TEXT,
  language    TEXT,
  publisher   TEXT,
  description TEXT,
  published   TEXT,
  has_cover   INTEGER NOT NULL DEFAULT 0,
  added_at    INTEGER NOT NULL,
  source      TEXT,
  format      TEXT NOT NULL DEFAULT 'epub',
  -- Metadata editing: which columns the user hand-edited (JSON array), when, and
  -- a stable public slug that survives file rewrites (set once at import).
  edited_fields TEXT NOT NULL DEFAULT '',
  edited_at     INTEGER,
  slug_override TEXT
);

CREATE TABLE IF NOT EXISTS authors (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE IF NOT EXISTS book_authors (
  book_id   INTEGER REFERENCES books(id) ON DELETE CASCADE,
  author_id INTEGER REFERENCES authors(id) ON DELETE CASCADE,
  PRIMARY KEY (book_id, author_id)
);

CREATE TABLE IF NOT EXISTS series (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE IF NOT EXISTS book_series (
  book_id   INTEGER REFERENCES books(id) ON DELETE CASCADE,
  series_id INTEGER REFERENCES series(id) ON DELETE CASCADE,
  idx       REAL,
  PRIMARY KEY (book_id, series_id)
);

CREATE TABLE IF NOT EXISTS tags (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE IF NOT EXISTS book_tags (
  book_id INTEGER REFERENCES books(id) ON DELETE CASCADE,
  tag_id  INTEGER REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (book_id, tag_id)
);

CREATE TABLE IF NOT EXISTS identifiers (
  book_id INTEGER REFERENCES books(id) ON DELETE CASCADE,
  scheme  TEXT,
  value   TEXT
);

CREATE TABLE IF NOT EXISTS read_state (
  book_id    INTEGER PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
  percent    REAL,
  cfi        TEXT,
  updated_at INTEGER
);

CREATE VIRTUAL TABLE IF NOT EXISTS books_fts USING fts5(
  title, authors, description, content=''
);

CREATE INDEX IF NOT EXISTS idx_books_added ON books(added_at DESC);
CREATE INDEX IF NOT EXISTS idx_books_sort  ON books(sort_title);
`
