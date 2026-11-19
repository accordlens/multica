// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ChatCapabilitiesSchema, ChatConversationV2Schema, ChatMessagesV2Schema } from "./schemas";
import { parseWithFallback } from "./schema";

const valid={protocol:2,team_enabled:false,supported_kinds:["agent_dm"],private_download_identity_required:true,limits:{message_codepoints:40000,history_default:50,history_max:100,group_members:9,attachments:10,upload_bytes:104857600}};
describe("chat protocol capabilities",()=>{
 it("accepts the gated C2 fixture and additive fields",()=>{
  expect(ChatCapabilitiesSchema.parse({...valid,future:true}).team_enabled).toBe(false);
 });
 it.each([null,{}, {...valid,protocol:3},{...valid,team_enabled:"yes"},{...valid,supported_kinds:["unknown"]},{...valid,private_download_identity_required:false},{...valid,limits:{...valid.limits,history_max:-1}}])("fails closed for malformed response %j",(raw)=>{
  expect(parseWithFallback(raw,ChatCapabilitiesSchema,null,{endpoint:"GET /api/chat/v2/capabilities"})).toBeNull();
 });
});

describe("v2 conversation and history DTOs",()=>{
 const id="00000000-0000-4000-8000-000000000001";
 const conversation={id,workspace_id:id,kind:"private_channel",name:"team",topic:"",description:"",revision:1,acl_version:1,last_message_seq:1};
 const history={messages:[{id,chat_session_id:id,actor_type:"member",actor_id:id,content:"hello",message_seq:1,revision:1,root_message_id:null,deleted:false,created_at:"2026-10-08T00:00:00Z"}],next_cursor:"",snapshot_head:1};
 it("accepts server fixtures",()=>{expect(ChatConversationV2Schema.parse(conversation).kind).toBe("private_channel");expect(ChatMessagesV2Schema.parse(history).messages).toHaveLength(1)});
 it.each([{}, {...conversation,kind:"future"}, {...conversation,acl_version:0},{...conversation,id:"invalid"}])("rejects conversation %j",raw=>expect(ChatConversationV2Schema.safeParse(raw).success).toBe(false));
 it.each([{}, {...history,messages:null},{...history,snapshot_head:-1},{...history,messages:[{...history.messages[0],actor_id:"bad"}]}])("rejects history %j",raw=>expect(ChatMessagesV2Schema.safeParse(raw).success).toBe(false));
});
