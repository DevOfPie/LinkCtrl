-- +goose Up
--
-- Deep-link path forwarding (M33).
--
-- One column, and it deliberately looks exactly like `forward_query` two
-- migrations' worth of features earlier: a boolean, NOT NULL, defaulting to
-- false. The pair answers the same question about two halves of the same URL,
-- and giving the second half a different shape — a nullable boolean, a mode
-- string, a jsonb bag — would mean every reader has to learn which of two
-- spellings applies to which half.
--
-- Off for every row that exists when this runs, which is what makes the
-- migration safe to apply before the code that reads it. A link whose
-- destination is `https://shop.example/p/42` has never been asked what
-- `/{alias}/reviews` should mean, and inventing an answer would silently change
-- where a live link sends people.
--
-- No index. Nothing queries by it: the redirect path reads it from the row it
-- was already fetching, and the dashboard reads it from the link it is showing.
ALTER TABLE links
    ADD COLUMN forward_path boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE links DROP COLUMN IF EXISTS forward_path;
