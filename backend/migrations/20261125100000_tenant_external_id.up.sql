-- external_id is the tenant's ID in the HAMSP platform (the Zitadel organization ID);
-- in platform mode the tenant is resolved by it instead of by tenant_domains.
ALTER TABLE tenants ADD COLUMN external_id TEXT UNIQUE;
-- Set once a tenant created in platform mode has its defaults; until then every replica retries the (idempotent) seed.
ALTER TABLE tenants ADD COLUMN defaults_seeded_at TIMESTAMPTZ;
