// Types for /api/rooms/* (qunliao P0c). Mirrors the Go structs in
// internal/app/dock/chat_rooms_store.go.

export type RoomKind = "room" | "dm" | "whisper" | "convene";
export type RoomRole = "owner" | "member" | "observer";

export interface Room {
  id: number;
  workspace_id: string;
  kind: RoomKind;
  name: string;
  topic: string;
  created_by_user_id: string;
  created_at: string;
  archived_at?: string;
}

export interface RoomMember {
  room_id: number;
  user_id: string;
  role: RoomRole;
  joined_at: string;
  last_read_message_id?: number;
}

// RoomMessage reuses the same chat_messages row shape as 1:1 DMs, just
// with room_id set instead of (or alongside) thread_id.
export interface RoomMessage {
  id: number;
  room_id?: number;
  thread_id?: number;
  llm_thread_id?: number;
  sender_id: string;
  sender_username: string;
  sender_icon: string;
  message_type: string;
  failed: boolean;
  content: string;
  created_at: string;
  llm_config_id?: number;
  llm_config_name?: string;
  llm_model?: string;
}

export interface RoomListResponse {
  rooms: Room[];
}
export interface RoomGetResponse {
  room: Room;
  member: RoomMember;
}
export interface RoomMemberListResponse {
  members: RoomMember[];
}
export interface RoomMessageListResponse {
  messages: RoomMessage[];
}
export interface RoomMessageSendResponse {
  message: {
    id: number;
    room_id: number;
    sender_id: string;
    message_type: string;
    content: string;
    created_at: string;
  };
}
export interface RoomCreateResponse {
  room: Room;
}
