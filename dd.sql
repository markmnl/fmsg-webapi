-- fmsg-webapi database extensions.
--
-- This file extends the fmsgd schema at:
-- https://github.com/markmnl/fmsgd/blob/main/dd.sql
--
-- fmsg-webapi assumes fmsgd is the fmsg host implementation and uses the same
-- PostgreSQL database.

CREATE TABLE IF NOT EXISTS fmsg_api_sub_account (
    owner_addr varchar(255) NOT NULL,
    agent varchar(64) NOT NULL,
    sub_addr varchar(255),
    grant_type text NOT NULL DEFAULT 'derived_sub_account',
    display_name text,
    key_id varchar(64),
    key_hash bytea,
    allowed_cidrs cidr[],
    key_expires_at timestamptz,
    max_sub_accounts int NOT NULL DEFAULT 5,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_addr, agent),
    UNIQUE (key_id),
    CHECK (max_sub_accounts > 0),
    CHECK (grant_type IN ('derived_sub_account', 'delegated_identity')),
    CHECK (
        (agent = '' AND sub_addr IS NULL AND display_name IS NULL AND key_id IS NULL AND key_hash IS NULL AND allowed_cidrs IS NULL AND key_expires_at IS NULL)
        OR
        (agent <> '' AND sub_addr IS NOT NULL AND key_id IS NOT NULL AND key_hash IS NOT NULL AND allowed_cidrs IS NOT NULL AND cardinality(allowed_cidrs) > 0 AND key_expires_at IS NOT NULL)
    ),
    CHECK (agent = '' OR agent NOT LIKE '%\_%' ESCAPE '\')
);

ALTER TABLE fmsg_api_sub_account
    ADD COLUMN IF NOT EXISTS grant_type text NOT NULL DEFAULT 'derived_sub_account';

ALTER TABLE fmsg_api_sub_account
    ADD COLUMN IF NOT EXISTS display_name text;

ALTER TABLE fmsg_api_sub_account
    DROP CONSTRAINT IF EXISTS fmsg_api_sub_account_sub_addr_key;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'fmsg_api_sub_account_grant_type_check'
    ) THEN
        ALTER TABLE fmsg_api_sub_account
            ADD CONSTRAINT fmsg_api_sub_account_grant_type_check
            CHECK (grant_type IN ('derived_sub_account', 'delegated_identity'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS fmsg_api_sub_account_owner_idx
    ON fmsg_api_sub_account ((lower(owner_addr)));

CREATE INDEX IF NOT EXISTS fmsg_api_sub_account_sub_idx
    ON fmsg_api_sub_account ((lower(sub_addr)));

CREATE UNIQUE INDEX IF NOT EXISTS fmsg_api_sub_account_owner_sub_unique
    ON fmsg_api_sub_account ((lower(owner_addr)), (lower(sub_addr)))
    WHERE agent <> '';
