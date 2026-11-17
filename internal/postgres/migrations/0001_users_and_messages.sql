-- Accounts. The password itself is never stored, only its bcrypt hash.
CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "Alice" and "alice" count as the same name, so nobody can register
-- ALICE while alice exists. The index also makes login fast.
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));

-- Chat messages. The id doubles as the message order: identity values only
-- go up, so a bigger id means a later message.
CREATE TABLE messages (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room       TEXT        NOT NULL,
    user_id    BIGINT      NOT NULL REFERENCES users (id),
    text       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every history query is "messages in room X with id below Y, newest
-- first". This index stores rows sorted exactly that way, so Postgres can
-- jump straight to the right spot instead of scanning the whole table.
CREATE INDEX messages_room_id_idx ON messages (room, id);
