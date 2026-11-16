-- 0065_teams — 团队、成员与一次性邀请码（f12 团队与角色，docs/features/team-roles.md）
--
-- 团队内角色 owner / admin / member 与 users.role（站内 admin / member）是
-- 两套正交的概念：站内管理员不会自动成为任何团队的成员，团队 owner 也不是
-- 站内管理员。本迁移只建表，不写任何业务数据（AGENTS §6.4）。
--
-- 团队名【不做】全站唯一：唯一约束会让「建团队返回 409」变成跨租户探测
-- 团队名是否存在的探针。团队用 id 区分，名字只是展示字段。
CREATE TABLE teams (
  id         bigserial   PRIMARY KEY,
  name       text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT teams_name_len_check CHECK (char_length(name) BETWEEN 1 AND 64)
);
CREATE INDEX teams_name_lower ON teams (lower(name));
CREATE TRIGGER teams_updated_at BEFORE UPDATE ON teams
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 成员关系。每个团队恰好一个 owner：owner 只能经「转让」产生（应用层保证）。
CREATE TABLE team_members (
  team_id    bigint      NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       text        NOT NULL DEFAULT 'member',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, user_id),
  CONSTRAINT team_members_role_check CHECK (role IN ('owner','admin','member'))
);
CREATE INDEX team_members_user ON team_members (user_id);
CREATE TRIGGER team_members_updated_at BEFORE UPDATE ON team_members
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 一次性邀请码。库里只存 sha256(明文码) 与末 4 位提示，明文只在创建响应里
-- 出现一次。邀请【不按邮箱绑定】：Lathe 开放注册、不验证邮箱，邮箱不是
-- 身份证明；invitee_email 只是给 owner 看的备注，不参与任何判定。
CREATE TABLE team_invitations (
  id            bigserial   PRIMARY KEY,
  team_id       bigint      NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  code_hash     bytea       NOT NULL,
  code_hint     text        NOT NULL,
  invitee_email text,
  role          text        NOT NULL DEFAULT 'member',
  invited_by    bigint      REFERENCES users(id) ON DELETE SET NULL,
  state         text        NOT NULL DEFAULT 'pending',
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  decided_at    timestamptz,
  accepted_by   bigint      REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT team_invitations_role_check  CHECK (role IN ('admin','member')),
  CONSTRAINT team_invitations_state_check CHECK (state IN ('pending','accepted','revoked'))
);
CREATE UNIQUE INDEX team_invitations_code_hash ON team_invitations (code_hash);
CREATE INDEX team_invitations_team_pending ON team_invitations (team_id) WHERE state = 'pending';
