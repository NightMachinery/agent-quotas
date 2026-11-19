import type { PluginClientContext } from "@getpaseo/plugin/client";
import { QuotaPanel } from "./client/panel";

export default function contribute(client: PluginClientContext) {
  const removePanel = client.addAgentPanel({
    id: "quotas",
    title: "Agent quotas",
    icon: "Gauge",
    Component: QuotaPanel,
  });
  const removeCommand = client.addCommandCenterItem({
    id: "open-quotas",
    title: "Agent quotas",
    icon: "Gauge",
    keywords: ["auto-continue", "cache", "standby", "compact", "usage limit"],
    context: "agent",
    onSelect: ({ openPanel }) => openPanel("quotas"),
  });
  return () => {
    removeCommand();
    removePanel();
  };
}
