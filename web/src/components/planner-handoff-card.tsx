"use client";

import * as React from "react";

import { RefreshCwIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { PlannerHandoffContent, PlannerHandoffSnapshot, PlannerHandoffView } from "@/lib/types";

const saveLabels: Record<PlannerHandoffView["status"]["state"], string> = {
  disabled: "保存已关闭",
  unsubmitted: "未提交",
  submitted: "已提交",
  failed: "保存失败",
  unknown: "提交结果待确认",
};
const recoveryLabels: Record<PlannerHandoffView["status"]["recovery_state"], string> = {
  disabled: "恢复注入已关闭",
  none: "尚未恢复",
  restored: "已核对恢复",
  degraded: "恢复降级，需核对",
};
const todoLabels = { pending: "待办", in_progress: "进行中", completed: "已记录完成" };
const when = (value?: string) => (value ? new Date(value).toLocaleString() : "—");

function HandoffContent({ content }: { content: PlannerHandoffContent }) {
  const background = [
    ["决策背景", content.decision],
    ["暂缓原因", content.deferred_reason],
    ["重新考虑条件", content.reconsider_when],
    ["下一步依赖", content.next_dependencies],
  ].filter((entry) => entry[1]);
  return (
    <div className="space-y-3 text-sm">
      {content.background_generated_at && (
        <p className="text-muted-foreground text-xs">背景生成 {when(content.background_generated_at)}</p>
      )}
      {background.map(([label, text]) => (
        <div key={label}>
          <p className="text-muted-foreground text-xs">{label}</p>
          <p className="whitespace-pre-wrap break-words">{text}</p>
        </div>
      ))}
      {content.background_missing && (
        <p className="text-muted-foreground">未提供仍适用的决策背景，按当前任务图核对。</p>
      )}
      <ul className="space-y-2">
        {(content.todos ?? []).map((todo) => (
          <li key={todo.id} className="rounded-md border p-2">
            <span className="text-muted-foreground text-xs">
              {todoLabels[todo.status]}
              {todo.needs_review ? " · 待核对" : ""}
            </span>
            <p className="whitespace-pre-wrap break-words">{todo.content}</p>
            {todo.references?.map((ref, index) => (
              <p
                key={`${ref.kind}-${ref.id}-${index}`}
                className={`text-xs ${ref.available ? "text-muted-foreground" : "text-amber-600 dark:text-amber-400"}`}
              >
                {ref.kind === "node" ? "任务节点" : "发现"} #{ref.id} · v{ref.version ?? 0}
                {!ref.available ? ` · ${ref.reason || "引用不可用"}` : ""}
              </p>
            ))}
          </li>
        ))}
      </ul>
    </div>
  );
}

function SnapshotTimes({ snapshot }: { snapshot: PlannerHandoffSnapshot }) {
  return (
    <p className="text-muted-foreground text-xs">
      内容生成 {when(snapshot.generated_at)} · 提交 {when(snapshot.committed_at)} · 最近核对 {when(snapshot.checked_at)}
    </p>
  );
}

export function PlannerHandoffCard({ taskId }: { taskId: string }) {
  const [view, setView] = React.useState<PlannerHandoffView | null>(null);
  const [error, setError] = React.useState("");
  const [history, setHistory] = React.useState(false);
  const reload = React.useRef<() => void>(() => undefined);
  React.useEffect(() => {
    let disposed = false;
    let busy = false;
    setView(null);
    const load = async () => {
      if (busy) return;
      busy = true;
      try {
        const next = await api.plannerHandoff(taskId, history);
        if (!disposed) {
          setView(next);
          setError("");
        }
      } catch (reason) {
        if (!disposed) {
          setView(null);
          setError(reason instanceof Error ? reason.message : "无法读取交接状态");
        }
      } finally {
        busy = false;
      }
    };
    reload.current = () => {
      void load();
    };
    void load();
    const timer = window.setInterval(load, 10000);
    return () => {
      disposed = true;
      reload.current = () => undefined;
      window.clearInterval(timer);
    };
  }, [taskId, history]);
  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between gap-3">
          <CardTitle className="text-base">Planner 交接</CardTitle>
          <Button variant="ghost" size="sm" aria-label="刷新 Planner 交接" onClick={() => reload.current()}>
            <RefreshCwIcon className="size-3.5" />
          </Button>
        </div>
        <CardDescription>
          {view
            ? `${saveLabels[view.status.state]} · ${recoveryLabels[view.status.recovery_state]}${view.snapshot ? ` · 最近成功版本 v${view.snapshot.version}` : ""}`
            : error || "正在读取…"}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {view && (
          <>
            {(view.status.last_error || view.status.recovery_reason || view.projection_error) && (
              <p className="text-amber-600 text-sm dark:text-amber-400">
                {[view.status.last_error, view.status.recovery_reason, view.projection_error]
                  .filter(Boolean)
                  .join("；")}
              </p>
            )}
            {view.snapshot ? (
              <>
                <SnapshotTimes snapshot={view.snapshot} />
                <details>
                  <summary className="cursor-pointer text-sm">查看待办与背景（按当前任务核对）</summary>
                  <div className="mt-3">
                    {view.projection ? (
                      <HandoffContent content={view.projection} />
                    ) : (
                      <p className="text-muted-foreground text-sm">暂时无法核对交接，未注入旧背景。</p>
                    )}
                  </div>
                </details>
                <Button variant="outline" size="sm" onClick={() => setHistory((value) => !value)}>
                  {history ? "收起历史" : "查看提交历史"}
                </Button>
                {history && (
                  <div className="space-y-2">
                    {view.history.map((snapshot) => (
                      <details key={snapshot.version} className="rounded-md border p-3">
                        <summary className="cursor-pointer text-sm">
                          v{snapshot.version} · {when(snapshot.committed_at)}
                        </summary>
                        <div className="mt-3 space-y-3">
                          <SnapshotTimes snapshot={snapshot} />
                          <p className="text-muted-foreground text-xs">
                            历史内容仅供查看，引用与结论需按当前任务重新核对。
                          </p>
                          <HandoffContent content={snapshot.content} />
                        </div>
                      </details>
                    ))}
                  </div>
                )}
              </>
            ) : (
              <p className="text-muted-foreground text-sm">尚无已提交交接。可在系统设置中分别启用保存与恢复注入。</p>
            )}
            <p className="text-muted-foreground text-xs">
              待办工具的内存更新可能尚未提交。交接提供规划背景；目标状态、证据与检查预算以当前任务图为准。
            </p>
          </>
        )}
      </CardContent>
    </Card>
  );
}
