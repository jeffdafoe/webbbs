-- ZBBS-001: Initial schema - user, profile, zchat, and messenger entities
-- Already applied to database on 2026-02-07
-- Revised 2026-02-14: removed premature board/door/plugin/thread/post tables

CREATE TABLE "user" (id UUID NOT NULL, username VARCHAR(35) NOT NULL, email VARCHAR(180) NOT NULL, roles JSON NOT NULL, password VARCHAR(255) NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, last_login_at TIMESTAMP(0) WITHOUT TIME ZONE DEFAULT NULL, is_verified BOOLEAN NOT NULL, is_active BOOLEAN NOT NULL, PRIMARY KEY (id));
CREATE UNIQUE INDEX UNIQ_8D93D649F85E0677 ON "user" (username);
CREATE UNIQUE INDEX UNIQ_8D93D649E7927C74 ON "user" (email);

CREATE TABLE user_profile (id UUID NOT NULL, alias VARCHAR(35) DEFAULT NULL, gender VARCHAR(10) NOT NULL, entry_message TEXT DEFAULT NULL, exit_message TEXT DEFAULT NULL, bio TEXT DEFAULT NULL, avatar_url VARCHAR(255) DEFAULT NULL, preferred_color VARCHAR(1) DEFAULT NULL, timezone VARCHAR(50) DEFAULT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, updated_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, user_id UUID NOT NULL, PRIMARY KEY (id));
CREATE UNIQUE INDEX UNIQ_D95AB405A76ED395 ON user_profile (user_id);

CREATE TABLE zchat_action (id UUID NOT NULL, slug VARCHAR(30) NOT NULL, action_list_slug VARCHAR(50) NOT NULL, template_self VARCHAR(255) NOT NULL, template_target VARCHAR(255) NOT NULL, template_no_target VARCHAR(255) NOT NULL, is_active BOOLEAN NOT NULL, sort_order INT NOT NULL, PRIMARY KEY (id));
CREATE INDEX idx_zchat_action_list ON zchat_action (action_list_slug, is_active);
CREATE UNIQUE INDEX zchat_unique_action_slug ON zchat_action (slug);

CREATE TABLE zchat_invite (id UUID NOT NULL, expires_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, is_accepted BOOLEAN DEFAULT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, room_id UUID NOT NULL, invited_user_id UUID NOT NULL, invited_by_id UUID NOT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_7E1139E054177093 ON zchat_invite (room_id);
CREATE INDEX IDX_7E1139E0C58DAD6E ON zchat_invite (invited_user_id);
CREATE INDEX IDX_7E1139E0A7B4A7E3 ON zchat_invite (invited_by_id);
CREATE INDEX idx_zchat_invite_user ON zchat_invite (invited_user_id, is_accepted);

CREATE TABLE zchat_message (id UUID NOT NULL, message_type VARCHAR(20) NOT NULL, content TEXT NOT NULL, metadata JSON DEFAULT NULL, is_deleted BOOLEAN NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, room_id UUID NOT NULL, sender_id UUID NOT NULL, target_user_id UUID DEFAULT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_EB9665954177093 ON zchat_message (room_id);
CREATE INDEX IDX_EB96659F624B39D ON zchat_message (sender_id);
CREATE INDEX IDX_EB966596C066AFE ON zchat_message (target_user_id);
CREATE INDEX idx_zchat_message_room_order ON zchat_message (room_id, created_at);
CREATE INDEX idx_zchat_message_sender_order ON zchat_message (sender_id, created_at);

CREATE TABLE zchat_presence (id UUID NOT NULL, status VARCHAR(20) NOT NULL, display_alias VARCHAR(35) DEFAULT NULL, is_invisible BOOLEAN NOT NULL, last_activity_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, connected_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, mercure_connection_id VARCHAR(255) DEFAULT NULL, user_id UUID NOT NULL, room_id UUID DEFAULT NULL, PRIMARY KEY (id));
CREATE UNIQUE INDEX UNIQ_BBC2460EA76ED395 ON zchat_presence (user_id);
CREATE INDEX idx_zchat_presence_room ON zchat_presence (room_id);
CREATE INDEX idx_zchat_presence_activity ON zchat_presence (last_activity_at);

CREATE TABLE zchat_private_message (id UUID NOT NULL, content TEXT NOT NULL, is_read BOOLEAN NOT NULL, read_at TIMESTAMP(0) WITHOUT TIME ZONE DEFAULT NULL, is_deleted_by_sender BOOLEAN NOT NULL, is_deleted_by_recipient BOOLEAN NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, sender_id UUID NOT NULL, recipient_id UUID NOT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_837E4A04F624B39D ON zchat_private_message (sender_id);
CREATE INDEX IDX_837E4A04E92F8F78 ON zchat_private_message (recipient_id);
CREATE INDEX idx_zchat_pm_conversation ON zchat_private_message (sender_id, recipient_id, created_at);
CREATE INDEX idx_zchat_pm_unread ON zchat_private_message (recipient_id, is_read);

CREATE TABLE zchat_room (id UUID NOT NULL, slug VARCHAR(50) NOT NULL, name VARCHAR(50) NOT NULL, description TEXT DEFAULT NULL, room_type VARCHAR(20) NOT NULL, min_security_level INT NOT NULL, max_security_level INT NOT NULL, min_age INT DEFAULT NULL, max_age INT DEFAULT NULL, no_access_can_see BOOLEAN NOT NULL, action_list_slug VARCHAR(50) NOT NULL, max_users INT DEFAULT NULL, is_active BOOLEAN NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, updated_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, room_leader_id UUID DEFAULT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_30EBA553EFBCAEBD ON zchat_room (room_leader_id);
CREATE UNIQUE INDEX zchat_unique_room_slug ON zchat_room (slug);

CREATE TABLE zchat_squelch (id UUID NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, user_id UUID NOT NULL, squelched_user_id UUID NOT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_4928164EA76ED395 ON zchat_squelch (user_id);
CREATE INDEX IDX_4928164E13BD9B9C ON zchat_squelch (squelched_user_id);
CREATE UNIQUE INDEX zchat_unique_user_squelch ON zchat_squelch (user_id, squelched_user_id);

CREATE TABLE zchat_trivia_game (id UUID NOT NULL, status VARCHAR(20) NOT NULL, current_round INT NOT NULL, max_rounds INT NOT NULL, question_started_at TIMESTAMP(0) WITHOUT TIME ZONE DEFAULT NULL, question_timeout INT NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, ended_at TIMESTAMP(0) WITHOUT TIME ZONE DEFAULT NULL, room_id UUID NOT NULL, current_question_id UUID DEFAULT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_642B55C954177093 ON zchat_trivia_game (room_id);
CREATE INDEX IDX_642B55C9A0F35D66 ON zchat_trivia_game (current_question_id);
CREATE INDEX idx_zchat_trivia_game_room ON zchat_trivia_game (room_id, status);

CREATE TABLE zchat_trivia_player (id UUID NOT NULL, personal_score INT NOT NULL, correct_answers INT NOT NULL, joined_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, game_id UUID NOT NULL, team_id UUID DEFAULT NULL, user_id UUID NOT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_5FC82364E48FD905 ON zchat_trivia_player (game_id);
CREATE INDEX IDX_5FC82364296CD8AE ON zchat_trivia_player (team_id);
CREATE INDEX IDX_5FC82364A76ED395 ON zchat_trivia_player (user_id);
CREATE UNIQUE INDEX zchat_trivia_unique_game_player ON zchat_trivia_player (game_id, user_id);

CREATE TABLE zchat_trivia_question (id UUID NOT NULL, category VARCHAR(100) DEFAULT NULL, question TEXT NOT NULL, answers JSON NOT NULL, difficulty INT NOT NULL, is_active BOOLEAN NOT NULL, times_asked INT NOT NULL, times_answered INT NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, PRIMARY KEY (id));
CREATE INDEX idx_zchat_trivia_question_category ON zchat_trivia_question (category);
CREATE INDEX idx_zchat_trivia_question_active ON zchat_trivia_question (is_active);

CREATE TABLE zchat_trivia_team (id UUID NOT NULL, name VARCHAR(50) NOT NULL, score INT NOT NULL, game_id UUID NOT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_83E0C25AE48FD905 ON zchat_trivia_team (game_id);

CREATE TABLE messenger_messages (id BIGINT GENERATED BY DEFAULT AS IDENTITY NOT NULL, body TEXT NOT NULL, headers TEXT NOT NULL, queue_name VARCHAR(190) NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, available_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, delivered_at TIMESTAMP(0) WITHOUT TIME ZONE DEFAULT NULL, PRIMARY KEY (id));
CREATE INDEX IDX_75EA56E0FB7336F0E3BD61CE16BA31DBBF396750 ON messenger_messages (queue_name, available_at, delivered_at, id);

ALTER TABLE user_profile ADD CONSTRAINT FK_D95AB405A76ED395 FOREIGN KEY (user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_invite ADD CONSTRAINT FK_7E1139E054177093 FOREIGN KEY (room_id) REFERENCES zchat_room (id) NOT DEFERRABLE;
ALTER TABLE zchat_invite ADD CONSTRAINT FK_7E1139E0C58DAD6E FOREIGN KEY (invited_user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_invite ADD CONSTRAINT FK_7E1139E0A7B4A7E3 FOREIGN KEY (invited_by_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_message ADD CONSTRAINT FK_EB9665954177093 FOREIGN KEY (room_id) REFERENCES zchat_room (id) NOT DEFERRABLE;
ALTER TABLE zchat_message ADD CONSTRAINT FK_EB96659F624B39D FOREIGN KEY (sender_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_message ADD CONSTRAINT FK_EB966596C066AFE FOREIGN KEY (target_user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_presence ADD CONSTRAINT FK_BBC2460EA76ED395 FOREIGN KEY (user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_presence ADD CONSTRAINT FK_BBC2460E54177093 FOREIGN KEY (room_id) REFERENCES zchat_room (id) NOT DEFERRABLE;
ALTER TABLE zchat_private_message ADD CONSTRAINT FK_837E4A04F624B39D FOREIGN KEY (sender_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_private_message ADD CONSTRAINT FK_837E4A04E92F8F78 FOREIGN KEY (recipient_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_room ADD CONSTRAINT FK_30EBA553EFBCAEBD FOREIGN KEY (room_leader_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_squelch ADD CONSTRAINT FK_4928164EA76ED395 FOREIGN KEY (user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_squelch ADD CONSTRAINT FK_4928164E13BD9B9C FOREIGN KEY (squelched_user_id) REFERENCES "user" (id) NOT DEFERRABLE;
ALTER TABLE zchat_trivia_game ADD CONSTRAINT FK_642B55C954177093 FOREIGN KEY (room_id) REFERENCES zchat_room (id) NOT DEFERRABLE;
ALTER TABLE zchat_trivia_game ADD CONSTRAINT FK_642B55C9A0F35D66 FOREIGN KEY (current_question_id) REFERENCES zchat_trivia_question (id) NOT DEFERRABLE;
ALTER TABLE zchat_trivia_player ADD CONSTRAINT FK_5FC82364E48FD905 FOREIGN KEY (game_id) REFERENCES zchat_trivia_game (id) NOT DEFERRABLE;
ALTER TABLE zchat_trivia_player ADD CONSTRAINT FK_5FC82364296CD8AE FOREIGN KEY (team_id) REFERENCES zchat_trivia_team (id) NOT DEFERRABLE;
ALTER TABLE zchat_trivia_player ADD CONSTRAINT FK_5FC82364A76ED395 FOREIGN KEY (user_id) REFERENCES "user" (id) NOT DEFERRABLE;

ALTER TABLE zchat_trivia_team ADD CONSTRAINT FK_83E0C25AE48FD905 FOREIGN KEY (game_id) REFERENCES zchat_trivia_game (id) NOT DEFERRABLE;
