export type TeamKind = "personal" | "team";
export type TeamRole = "owner" | "admin" | "member" | "viewer";

export type Team = {
  id: string;
  slug: string;
  name: string;
  description: string;
  avatar_url: string;
  kind: TeamKind;
  owner_user_id: string;
  created_at: string;
  updated_at: string;
};

export type TeamWithRole = Team & {
  my_role: TeamRole;
};

export type TeamMember = {
  team_id: string;
  user_id: string;
  username: string;
  email: string;
  icon_url: string;
  role: TeamRole;
  joined_at: string;
};

export type TeamListResponse = {
  teams: TeamWithRole[];
};

export type TeamDetailResponse = {
  team: Team;
  my_role: TeamRole;
};

export type TeamCreateResponse = {
  team: Team;
  my_role: TeamRole;
};

export type TeamUpdateResponse = {
  team: Team;
};

export type TeamMembersResponse = {
  members: TeamMember[];
};

export type TeamCreatePayload = {
  name: string;
  description?: string;
  avatar_url?: string;
};

export type TeamUpdatePayload = {
  name: string;
  description: string;
  avatar_url: string;
};
