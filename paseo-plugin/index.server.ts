import type { PluginServerContext } from "@getpaseo/plugin/server";
import { change, read } from "./server/quotas";
import { changeAgent, readAgent } from "./shared/quotas";

export default function contribute(server: PluginServerContext) {
  server.handle(readAgent, read);
  server.handle(changeAgent, change);
  return () => {};
}
