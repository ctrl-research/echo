-- +goose Up

-- citext: case-insensitive usernames and genre names without lower() wrappers
-- on every lookup and index.
CREATE EXTENSION IF NOT EXISTS citext;

-- pg_trgm: trigram similarity for fuzzy and substring search. This is what
-- makes "radiohed" find Radiohead and "beatles" find "The Beatles".
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- unaccent: fold diacritics so "bjork" matches "Björk".
CREATE EXTENSION IF NOT EXISTS unaccent;

-- unaccent() is only STABLE because it depends on a dictionary that could be
-- reloaded. Generated columns require IMMUTABLE, so wrap it. This is safe as
-- long as the dictionary is not changed under a populated index; changing it
-- would require a REINDEX regardless.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_unaccent(text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
AS $$ SELECT public.unaccent('public.unaccent'::regdictionary, $1) $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS immutable_unaccent(text);
DROP EXTENSION IF EXISTS unaccent;
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
