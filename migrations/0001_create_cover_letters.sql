-- Cover letters table.
--
-- One row per generation request. The row is the durable record of the whole
-- asynchronous workflow: it starts as `created`, moves to `processing` when a
-- worker picks it up, and ends as `completed` or `failed`.

CREATE TABLE IF NOT EXISTS cover_letters (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    job_url           TEXT     NOT NULL DEFAULT '',
    job_description   TEXT     NOT NULL DEFAULT '',
    tone              TEXT     NOT NULL DEFAULT '',
    resume_path       TEXT     NOT NULL DEFAULT '',
    resume_filename   TEXT     NOT NULL DEFAULT '',
    resume_text       TEXT     NOT NULL DEFAULT '',
    cover_letter_text TEXT     NOT NULL DEFAULT '',
    candidate_name    TEXT     NOT NULL DEFAULT '',
    pdf_path          TEXT     NOT NULL DEFAULT '',
    status            TEXT     NOT NULL DEFAULT 'created',
    error_message     TEXT     NOT NULL DEFAULT '',
    -- JSON-encoded AI validation verdict; empty when validation did not run.
    validation        TEXT     NOT NULL DEFAULT '',
    created_at        DATETIME,
    updated_at        DATETIME
);

-- The worker polls for work by status; the API looks up by primary key.
CREATE INDEX IF NOT EXISTS idx_cover_letters_status ON cover_letters (status);

-- Supports "most recent first" listings and retention sweeps.
CREATE INDEX IF NOT EXISTS idx_cover_letters_created_at ON cover_letters (created_at DESC);