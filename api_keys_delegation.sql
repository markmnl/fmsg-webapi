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

CREATE UNIQUE INDEX IF NOT EXISTS fmsg_api_sub_account_owner_sub_unique
    ON fmsg_api_sub_account ((lower(owner_addr)), (lower(sub_addr)))
    WHERE agent <> '';
