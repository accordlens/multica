// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ChatCapabilitiesSchema } from "./schemas";
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
