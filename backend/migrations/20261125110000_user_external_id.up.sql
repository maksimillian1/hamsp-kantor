-- external_id is the user's ID in the HAMSP platform (the Zitadel user ID); in platform mode the user is resolved by it.
ALTER TABLE users ADD COLUMN external_id TEXT;
ALTER TABLE users ADD CONSTRAINT uq_users_tenant_external_id UNIQUE (tenant_id, external_id);
