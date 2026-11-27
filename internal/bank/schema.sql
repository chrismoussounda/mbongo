CREATE TABLE accounts (
    id       INTEGER PRIMARY KEY,
    operator TEXT    NOT NULL CHECK (operator IN ('mtn', 'airtel')),
    kind     TEXT    NOT NULL CHECK (kind IN ('customer', 'merchant', 'mint')),
    name     TEXT    NOT NULL,
    currency TEXT    NOT NULL,
    balance  INTEGER NOT NULL,
    UNIQUE (operator, kind, name, currency),
    CHECK (kind = 'mint' OR balance >= 0)
);

CREATE TABLE transactions (
    id          TEXT    PRIMARY KEY,
    operator    TEXT    NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('collection', 'payout')),
    merchant_id INTEGER NOT NULL REFERENCES accounts (id),
    customer_id INTEGER NOT NULL REFERENCES accounts (id),
    amount      INTEGER NOT NULL CHECK (amount > 0),
    currency    TEXT    NOT NULL,
    partner_ref TEXT    NOT NULL,
    external_id TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('pending', 'successful', 'failed', 'expired')),
    reason      TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    expires_at  TEXT    NOT NULL,
    decided_at  TEXT,
    UNIQUE (merchant_id, partner_ref)
);

CREATE TABLE entries (
    id             INTEGER PRIMARY KEY,
    transaction_id TEXT    REFERENCES transactions (id),
    account_id     INTEGER NOT NULL REFERENCES accounts (id),
    amount         INTEGER NOT NULL CHECK (amount <> 0),
    at             TEXT    NOT NULL
);

CREATE INDEX entries_account ON entries (account_id);
