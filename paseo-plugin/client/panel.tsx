import { useRpc, type PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { ScrollView } from "@getpaseo/plugin/client/ui";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pressable, Text, View } from "react-native";
import { changeAgent, readAgent, type Action } from "../shared/quotas";

type Theme = PluginAgentPanelProps["theme"];

function Control({ label, onPress, disabled, selected, theme }: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  selected?: boolean;
  theme: Theme;
}) {
  const { colors } = theme;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled, selected }}
      disabled={disabled}
      onPress={onPress}
      style={({ pressed }) => ({
        minHeight: 44,
        justifyContent: "center",
        paddingHorizontal: 14,
        borderRadius: 8,
        borderWidth: 1,
        borderColor: selected ? colors.foreground : colors.border,
        backgroundColor: colors.surface1,
        opacity: pressed || (disabled && !selected) ? 0.55 : 1,
      })}
    >
      <Text style={{ color: colors.foreground, fontWeight: selected ? "600" : "400" }}>{label}</Text>
    </Pressable>
  );
}

const row = { flexDirection: "row", flexWrap: "wrap", gap: 8 } as const;
const cacheChoices = [
  { cache: "off", label: "Off" },
  { cache: "compact", label: "Compact" },
  { cache: "warm", label: "Warm" },
] as const;

function AgentControls({ agentId, theme, compact }: { agentId: string; theme: Theme; compact: boolean }) {
  const read = useRpc(readAgent);
  const change = useRpc(changeAgent);
  const queryClient = useQueryClient();
  const queryKey = ["agent-quotas", agentId];
  const mutation = useMutation({
    mutationFn: (action: Action) => change({ agentId, action }),
    onMutate: () => queryClient.cancelQueries({ queryKey }),
    onSuccess: (result) => {
      if (result.kind === "settings") {
        queryClient.setQueryData(queryKey, result.document);
      } else {
        void queryClient.invalidateQueries({ queryKey });
      }
    },
  });
  const status = useQuery({
    queryKey,
    queryFn: () => read({ agentId }),
    retry: false,
    refetchInterval: mutation.isPending ? false : 15_000,
  });
  const doc = status.data;
  const agent = doc?.agent;
  const auto = agent?.auto_continue;
  const policy = agent?.cache?.policy ?? "off";
  const { colors } = theme;
  const disabled = mutation.isPending || !agent || agent.archived || status.isError;
  const cacheDisabled = disabled || !agent?.keeper_enabled;
  const seat = agent?.seat;
  const next = agent?.next_action;
  const at = next?.at == null ? "" : ` at ${new Date(next.at * 1000).toLocaleString()}`;
  const problem = status.error?.message ?? mutation.error?.message;
  const heading = { color: colors.foreground, fontSize: 16, fontWeight: "600" } as const;
  const muted = { color: colors.foregroundMuted };
  return (
    <ScrollView
      style={{ flex: 1, backgroundColor: colors.surface0 }}
      contentContainerStyle={{ padding: compact ? 16 : 24, gap: 20 }}
    >
      {status.isPending && <Text style={muted}>Loading agent quotas…</Text>}
      {problem && (
        <View style={{ gap: 8 }}>
          <Text accessibilityRole="alert" style={{ color: colors.foreground }}>{problem}</Text>
          {status.isError && <View style={row}>
            <Control label="Retry" theme={theme} disabled={status.isFetching} onPress={() => void status.refetch()} />
          </View>}
        </View>
      )}
      {doc && agent && (
        <>
          <View style={{ gap: 5 }}>
            <Text style={{ color: colors.foreground }}>
              {seat ? `Seat quota: ${seat.verdict} (${seat.account})` : "Seat quota: unknown"}
            </Text>
            {agent.archived && <Text style={muted}>This agent is archived.</Text>}
          </View>
          <View style={{ gap: 8 }}>
            <Text style={heading}>Auto-continue</Text>
            <Text style={muted}>
              {auto?.enabled ? "On" : "Off"}{auto?.status && auto.status !== "off" ? ` · ${auto.status}` : ""}
              {auto?.set_by ? ` · set by ${auto.set_by}` : ""}
            </Text>
            {!agent.auto_continue_engine_enabled && <Text style={muted}>The Paseo auto-continue engine is disabled.</Text>}
            <View style={row}>
              <Control label="On" theme={theme} selected={auto?.enabled === true}
                disabled={disabled || auto?.enabled === true}
                onPress={() => mutation.mutate({ kind: "auto-continue", enabled: true })} />
              <Control label="Off" theme={theme} selected={!auto?.enabled}
                disabled={disabled || (!auto?.enabled && Boolean(auto?.set_by))}
                onPress={() => mutation.mutate({ kind: "auto-continue", enabled: false })} />
            </View>
          </View>
          <View style={{ gap: 8 }}>
            <Text style={heading}>Cache policy</Text>
            <Text style={muted}>
              {agent.keeper_enabled
                ? `Phase: ${agent.cache?.phase ?? "off"}. Next: ${next?.kind ?? "none"}${at}${next?.reason ? ` · ${next.reason}` : ""}`
                : "The cache keeper is disabled on this host."}
            </Text>
            <View style={row}>
              {cacheChoices.map(({ cache, label }) => (
                <Control key={cache} label={label} theme={theme} selected={policy === cache}
                  disabled={cacheDisabled || (policy === cache && agent.cache?.phase !== "standby")}
                  onPress={() => mutation.mutate({ kind: "policy", cache })} />
              ))}
            </View>
            <View style={row}>
              <Control label="Standby" theme={theme} disabled={cacheDisabled || agent.cache?.phase === "standby"}
                onPress={() => mutation.mutate({ kind: "standby" })} />
              <Control label="Compact now" theme={theme} disabled={cacheDisabled}
                onPress={() => mutation.mutate({ kind: "compact" })} />
            </View>
            {mutation.data?.kind === "compaction" && (
              <Text style={muted}>Compaction request: {mutation.data.state}</Text>
            )}
          </View>
        </>
      )}
    </ScrollView>
  );
}

export function QuotaPanel({ params, theme, layout }: PluginAgentPanelProps) {
  return <AgentControls key={params.agentId} agentId={params.agentId} theme={theme} compact={layout.compact} />;
}
