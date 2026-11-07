-- +goose Up

-- Admins run the admin panel. The flag is granted with `wearwhat make-admin <email>`.
ALTER TABLE users ADD COLUMN is_admin boolean NOT NULL DEFAULT false;

-- What admins did, from the panel or the admin tool. actor is "@handle" or "cli".
CREATE TABLE admin_actions (
  id     text PRIMARY KEY,
  actor  text NOT NULL,
  action text NOT NULL,
  target text NOT NULL,
  detail text NOT NULL DEFAULT '',
  at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_actions_at_idx ON admin_actions(at DESC);

-- +goose Down

DROP TABLE admin_actions;
ALTER TABLE users DROP COLUMN is_admin;
