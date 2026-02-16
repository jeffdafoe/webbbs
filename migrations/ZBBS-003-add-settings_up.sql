-- ZBBS-003: Settings table for BBS configuration

CREATE TABLE setting (
    key VARCHAR(100) NOT NULL,
    value TEXT DEFAULT NULL,
    description VARCHAR(255) DEFAULT NULL,
    is_public BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (key)
);

CREATE INDEX idx_setting_public ON setting (is_public);

INSERT INTO setting (key, value, description, is_public) VALUES
    ('bbs_name', 'ZBBS', 'Name of the BBS', true),
    ('bbs_tagline', 'Bulletin Board System', 'Tagline shown on welcome screen', true),
    ('bbs_phone', '555-ZBBS', 'Phone number shown in modem disconnect', true),
    ('default_role', 'ROLE_USER', 'Default role assigned to new users', false),
    ('registration_enabled', 'true', 'Whether new user registration is open', true);
