import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";

export const cachePolicy = z.enum(["off", "compact", "warm"]);

const agentID = z.object({ agentId: z.uuid() });
const autoContinue = z.object({ enabled: z.boolean(), status: z.string(), set_by: z.string().optional() });

export const agentDocument = z.object({
  schema_version: z.literal(1),
  generated_at: z.number(),
  agent: z.object({
    keeper_enabled: z.boolean(),
    auto_continue_engine_enabled: z.boolean(),
    archived: z.boolean(),
    cache: z.object({ policy: cachePolicy, phase: z.string() }).nullable(),
    auto_continue: autoContinue.nullable(),
    next_action: z.object({ kind: z.string(), at: z.number().nullable(), reason: z.string() }),
    seat: z.object({ account: z.string(), verdict: z.string() }).nullable(),
  }),
});

export const action = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("auto-continue"), enabled: z.boolean() }),
  z.object({ kind: z.literal("policy"), cache: cachePolicy }),
  z.object({ kind: z.literal("standby") }),
  z.object({ kind: z.literal("compact") }),
]);
export type Action = z.infer<typeof action>;

export const readAgent = defineRpc({
  name: "quotas.read-agent",
  input: agentID,
  output: agentDocument,
});

export const changeAgent = defineRpc({
  name: "quotas.change-agent",
  input: agentID.extend({ action }),
  output: z.discriminatedUnion("kind", [
    z.object({ kind: z.literal("settings"), document: agentDocument }),
    z.object({ kind: z.literal("compaction"), state: z.string() }),
  ]),
});
