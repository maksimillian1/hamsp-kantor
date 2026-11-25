ALTER TABLE users DROP CONSTRAINT uq_users_tenant_external_id;
ALTER TABLE users DROP COLUMN external_id;
