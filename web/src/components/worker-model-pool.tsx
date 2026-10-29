"use client";

import * as React from "react";

import { ArrowDownIcon, ArrowUpIcon, XIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxItem,
  ComboboxList,
  ComboboxValue,
} from "@/components/ui/combobox";
import type { LLMProfile } from "@/lib/types";

interface WorkerModelPoolProps {
  profiles: LLMProfile[];
  value: number[];
  onValueChange: (value: number[]) => void;
  workers: number;
  disabled?: boolean;
  profilesLoading?: boolean;
}

export function WorkerModelPool({
  profiles,
  value,
  onValueChange,
  workers,
  disabled = false,
  profilesLoading = false,
}: WorkerModelPoolProps) {
  const profilesByID = React.useMemo(() => new Map(profiles.map((profile) => [profile.id, profile])), [profiles]);
  const itemIDs = React.useMemo(() => profiles.map((profile) => profile.id), [profiles]);
  const selectedIDs = React.useMemo(() => value.map(String), [value]);
  const profileLabel = React.useCallback(
    (id: string) => {
      const profile = profilesByID.get(id);
      return profile ? `${profile.name} ${profile.model}` : `配置 #${id}`;
    },
    [profilesByID],
  );

  const move = (index: number, offset: -1 | 1) => {
    const target = index + offset;
    if (target < 0 || target >= value.length) return;
    const next = [...value];
    [next[index], next[target]] = [next[target], next[index]];
    onValueChange(next);
  };
  const previewCount = Number.isInteger(workers) && workers > 0 ? Math.min(workers, 6) : 3;
  const preview =
    value.length > 0
      ? Array.from({ length: previewCount }, (_, index) => ({
          slot: `work#${index + 1}`,
          id: value[index % value.length],
        }))
      : [];
  const profilesUnavailable = profilesLoading ? true : profiles.length === 0;

  return (
    <div className="flex flex-col gap-3">
      <Combobox
        items={itemIDs}
        itemToStringValue={profileLabel}
        multiple
        value={selectedIDs}
        onValueChange={(ids) => onValueChange(ids.map(Number))}
        disabled={disabled}
      >
        <ComboboxChips>
          <ComboboxValue>
            {selectedIDs.map((id) => (
              <ComboboxChip key={id}>{profilesByID.get(id)?.name ?? `#${id}`}</ComboboxChip>
            ))}
          </ComboboxValue>
          <ComboboxChipsInput
            id="worker-model-pool"
            placeholder={profilesLoading ? "正在加载 LLM 配置…" : "搜索并添加 LLM 配置（最多 32 项）"}
            disabled={disabled ? true : profilesUnavailable}
          />
        </ComboboxChips>
        <ComboboxContent>
          <ComboboxEmpty>没有匹配的 LLM 配置</ComboboxEmpty>
          <ComboboxList>
            {(id) => {
              const profile = profilesByID.get(id);
              return (
                <ComboboxItem key={id} value={id} disabled={value.length >= 32 && !selectedIDs.includes(id)}>
                  <div className="flex min-w-0 flex-1 flex-col">
                    <span className="truncate">
                      {profile?.name ?? `配置 #${id}`}
                      {profile?.is_default ? "（激活）" : ""}
                    </span>
                    <span className="truncate text-muted-foreground text-xs">{profile?.model}</span>
                  </div>
                </ComboboxItem>
              );
            }}
          </ComboboxList>
        </ComboboxContent>
      </Combobox>

      {value.length > 0 ? (
        <>
          <div className="flex flex-col divide-y rounded-lg border">
            {value.map((id, index) => {
              const profile = profilesByID.get(String(id));
              const name = profile?.name ?? `配置 #${id}`;
              return (
                <div key={id} className="flex min-w-0 items-center gap-2 px-2.5 py-2">
                  <span className="w-5 shrink-0 text-center text-muted-foreground text-xs tabular-nums">
                    {index + 1}
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className="truncate font-medium text-sm">{name}</p>
                    <p className="truncate text-muted-foreground text-xs">
                      {profile?.model ?? "配置已不可用，请移除后保存"}
                    </p>
                  </div>
                  <div className="flex shrink-0 gap-1">
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`上移${name}`}
                      onClick={() => move(index, -1)}
                      disabled={disabled ? true : index === 0}
                    >
                      <ArrowUpIcon />
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`下移${name}`}
                      onClick={() => move(index, 1)}
                      disabled={disabled ? true : index === value.length - 1}
                    >
                      <ArrowDownIcon />
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`移除${name}`}
                      onClick={() => onValueChange(value.filter((profileID) => profileID !== id))}
                      disabled={disabled}
                    >
                      <XIcon />
                    </Button>
                  </div>
                </div>
              );
            })}
          </div>
          <div className="rounded-md bg-muted/50 px-3 py-2 text-xs">
            <p className="mb-1 font-medium">分配预览{workers > previewCount ? `（前 ${previewCount} 个）` : ""}</p>
            <ul className="space-y-1 text-muted-foreground">
              {preview.map(({ slot, id }) => (
                <li key={slot}>
                  {slot} → {profilesByID.get(String(id))?.name ?? `配置 #${id}`}
                </li>
              ))}
            </ul>
          </div>
          <Button
            type="button"
            variant="outline"
            className="self-start"
            onClick={() => onValueChange([])}
            disabled={disabled}
          >
            清空模型池
          </Button>
        </>
      ) : (
        <p className="text-muted-foreground text-xs">
          {profiles.length === 0 && !profilesLoading
            ? "暂无 LLM 配置，请先在模型配置中添加。"
            : "模型池为空，沿用现有模型选择方式。"}
        </p>
      )}
    </div>
  );
}
