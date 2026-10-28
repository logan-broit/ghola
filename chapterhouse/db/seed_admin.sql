-- Seed admin user for Chapterhouse admin console
-- Password: admin123
-- Bcrypt hash generated with cost 10

INSERT INTO users (id, username, password_hash, is_admin, created_at, modified_at)
VALUES (
    gen_random_uuid(),
    'admin',
    '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy',
    true,
    NOW(),
    NOW()
)
ON CONFLICT (username) DO UPDATE SET
    password_hash = EXCLUDED.password_hash,
    is_admin = EXCLUDED.is_admin,
    modified_at = NOW();
