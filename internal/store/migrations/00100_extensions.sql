-- +goose Up

-- pg_trgm backs substring search on aliases and destination URLs.
--
-- Creating an extension needs privileges the application role may not have on
-- a managed Postgres. Rather than failing the whole migration there, this
-- degrades: without the extension, search falls back to full-text only and the
-- ILIKE branch becomes a sequential scan. Acceptable, and documented in
-- DEPLOY.md; a hard failure here would block self-hosting on several managed
-- providers for a feature that has a working fallback.
-- +goose StatementBegin
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION
    WHEN insufficient_privilege THEN
        RAISE WARNING 'pg_trgm could not be created (insufficient privilege). '
                      'Substring search will fall back to sequential scans. '
                      'Ask your database administrator to run: '
                      'CREATE EXTENSION pg_trgm;';
END
$$;
-- +goose StatementEnd

-- citext is deliberately NOT used for email. Case-insensitive comparison is
-- done explicitly with lower(), because citext's collation behaviour varies
-- across ICU versions and a silent change in equality semantics on the login
-- path is not a risk worth taking for the convenience.

-- +goose Down
DROP EXTENSION IF EXISTS pg_trgm;
