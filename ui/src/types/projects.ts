export type ProjectStatus =
  | "draft"
  | "planning"
  | "building"
  | "beta"
  | "shipped"
  | "archived";

export type FeatureComplexity = "simple" | "medium" | "complex";

export type TaskStatus = "todo" | "in_progress" | "review" | "done" | "blocked";

export type Project = {
  id: string;
  workspace_id: string;
  creator_user_id: string;
  name: string;
  description: string;
  status: ProjectStatus;
  template: string;
  created_at: string;
  updated_at: string;
};

export type ProjectFeature = {
  id: string;
  project_id: string;
  ord: number;
  title: string;
  description: string;
  complexity: FeatureComplexity;
  selected: boolean;
  created_at: string;
};

export type ProjectTask = {
  id: string;
  project_id: string;
  feature_id?: string;
  title: string;
  description: string;
  status: TaskStatus;
  bot_user_id?: string;
  chat_thread_id?: number;
  llm_thread_id?: number;
  ord: number;
  created_at: string;
  updated_at: string;
  started_at?: string;
  completed_at?: string;
};

export type ProjectListResponse = {
  projects: Project[];
};

export type ProjectDetailResponse = {
  project: Project;
  features: ProjectFeature[];
  tasks: ProjectTask[];
};

export type ProjectCreateResponse = {
  project: Project;
};

export type ProjectDecomposeResponse = {
  features: ProjectFeature[];
  error?: string;
  raw?: string;
  hint?: string;
};

export type ProjectGeneratePlanResponse = {
  tasks: ProjectTask[];
  created_count: number;
};

export type ProjectCreatePayload = {
  name: string;
  description?: string;
  template?: string;
};

export type ProjectUpdatePayload = {
  name?: string;
  description?: string;
  status?: ProjectStatus;
  template?: string;
};

export type ProjectFeatureUpdatePayload = {
  title: string;
  description: string;
  complexity: FeatureComplexity;
  selected: boolean;
};

export type ProjectTaskUpdatePayload = {
  title?: string;
  description?: string;
  status?: TaskStatus;
};

export type AgentToken = {
  id: string;
  user_id: string;
  name: string;
  last_used_at?: string;
  revoked_at?: string;
  created_at: string;
};

export type AgentTokenCreateResponse = {
  token: AgentToken;
  raw: string;
  hint: string;
};

export type AgentTokenListResponse = {
  tokens: AgentToken[];
};
