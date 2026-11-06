-- +goose Up

-- Where each public file is served from. It's stored because Cloudinary chooses the address,
-- and a posted URL has to match it exactly to count as your own upload.
ALTER TABLE uploads ADD COLUMN url text;
UPDATE uploads SET url = '/uploads/' || id WHERE is_public;
CREATE UNIQUE INDEX uploads_url_idx ON uploads(url) WHERE url IS NOT NULL;

-- +goose Down

DROP INDEX uploads_url_idx;
ALTER TABLE uploads DROP COLUMN url;
