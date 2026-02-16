-- ZBBS-004: Remove terminal border color setting

DELETE FROM setting WHERE key = 'terminal_border_color';
