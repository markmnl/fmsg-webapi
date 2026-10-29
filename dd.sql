-- fmsg-webapi database extensions.
--
-- This file extends the fmsgd schema at:
-- https://github.com/markmnl/fmsgd/blob/main/dd.sql
--
-- fmsg-webapi assumes fmsgd is the fmsg host implementation and uses the same
-- PostgreSQL database.
--
-- This script is IDEMPOTENT: every statement is safe to re-run (CREATE ... IF
-- NOT EXISTS, ALTER TABLE ... ADD COLUMN IF NOT EXISTS, DROP CONSTRAINT IF
-- EXISTS, guarded DO blocks). Migrating an existing database is therefore
-- just re-running the whole script, e.g.:
--
--   psql -d fmsgd -v ON_ERROR_STOP=1 -f dd.sql
--
-- Keep it that way: add new objects and columns only with idempotent
-- statements.

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

-- Trash: what each address has deleted. A row marks one message for one
-- address (lower-cased). thread marks the whole thread below that message;
-- time_purged means deleted for good: never shown to that address again,
-- nor anything that replies to it. Rows go with their message when it is
-- removed from the host.
CREATE TABLE IF NOT EXISTS fmsg_api_trash (
    addr text NOT NULL,
    msg_id bigint NOT NULL REFERENCES msg (id) ON DELETE CASCADE,
    thread boolean NOT NULL DEFAULT false,
    time_deleted double precision NOT NULL,
    time_purged double precision,
    PRIMARY KEY (addr, msg_id)
);

-- fmsg_api_trash_state is where a message stands for an address: 'purged'
-- when it or an ancestor is deleted for good, 'thread' when it or an ancestor
-- carries a thread mark, 'message' when it is deleted itself, NULL when it
-- isn't deleted. addr must be lower-cased.
CREATE OR REPLACE FUNCTION fmsg_api_trash_state(p_addr text, p_msg bigint) RETURNS text
LANGUAGE sql STABLE AS $$
    WITH RECURSIVE up(id, depth) AS (
        SELECT p_msg, 0
        UNION ALL
        SELECT m.pid, up.depth + 1 FROM msg m JOIN up ON m.id = up.id
        WHERE m.pid IS NOT NULL AND up.depth < 1000
    )
    SELECT CASE
        WHEN bool_or(t.time_purged IS NOT NULL) THEN 'purged'
        WHEN bool_or(t.thread) THEN 'thread'
        WHEN bool_or(up.depth = 0) THEN 'message'
    END
    FROM up JOIN fmsg_api_trash t ON t.addr = p_addr AND t.msg_id = up.id
$$;

-- fmsg_api_delivery_pending is whether fmsgd may still deliver some part of
-- message p_msg: a recipient (msg_to, msg_add_to) or an add-to participant
-- notification (msg_add_to_notify) that is not delivered, whose last attempt
-- (if any) got a code in p_retryable, and that at p_now is younger than
-- p_max_age seconds, measured from the message's time_sent or its add-to
-- batch's time_added as fmsgd's sender measures it. fmsgd gives up on older
-- rows without marking them, so they no longer keep a message on the host.
CREATE OR REPLACE FUNCTION fmsg_api_delivery_pending(p_msg bigint, p_now double precision, p_max_age double precision, p_retryable smallint[]) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM msg_to t JOIN msg m ON m.id = t.msg_id
        WHERE t.msg_id = p_msg AND m.time_sent IS NOT NULL
          AND t.time_delivered IS NULL
          AND (t.response_code IS NULL OR t.response_code = ANY (p_retryable))
          AND p_now - m.time_sent < p_max_age)
    OR EXISTS (
        SELECT 1 FROM msg_add_to a JOIN msg_add_to_batch b ON b.id = a.batch_id
        WHERE a.msg_id = p_msg AND a.time_delivered IS NULL
          AND (a.response_code IS NULL OR a.response_code = ANY (p_retryable))
          AND p_now - b.time_added < p_max_age)
    OR EXISTS (
        SELECT 1 FROM msg_add_to_notify n JOIN msg_add_to_batch b ON b.id = n.batch_id
        WHERE b.msg_id = p_msg AND n.time_notified IS NULL
          AND (n.response_code IS NULL OR n.response_code = ANY (p_retryable))
          AND p_now - b.time_added < p_max_age)
$$;
