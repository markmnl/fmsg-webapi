CREATE TABLE IF NOT EXISTS fmsg_api_sub_account (
    owner_addr varchar(255) NOT NULL,
    agent varchar(64) NOT NULL,
    sub_addr varchar(255),
    key_id varchar(64),
    key_hash bytea,
    allowed_cidrs cidr[],
    key_expires_at timestamptz,
    max_sub_accounts int NOT NULL DEFAULT 5,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_addr, agent),
    UNIQUE (sub_addr),
    UNIQUE (key_id),
    CHECK (max_sub_accounts > 0),
    CHECK (
        (agent = '' AND sub_addr IS NULL AND key_id IS NULL AND key_hash IS NULL AND allowed_cidrs IS NULL AND key_expires_at IS NULL)
        OR
        (agent <> '' AND sub_addr IS NOT NULL AND key_id IS NOT NULL AND key_hash IS NOT NULL AND allowed_cidrs IS NOT NULL AND cardinality(allowed_cidrs) > 0 AND key_expires_at IS NOT NULL)
    ),
    CHECK (agent = '' OR agent NOT LIKE '%\_%' ESCAPE '\')
);

CREATE INDEX IF NOT EXISTS fmsg_api_sub_account_owner_idx
    ON fmsg_api_sub_account ((lower(owner_addr)));

CREATE INDEX IF NOT EXISTS fmsg_api_sub_account_sub_idx
    ON fmsg_api_sub_account ((lower(sub_addr)));
