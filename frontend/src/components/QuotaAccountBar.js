import React, { useEffect, useState } from 'react';

const formatBytes = (bytes) => {
  if (bytes == null || bytes <= 0) {
    return '0 B';
  }
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${parseFloat((bytes / (1024 ** exponent)).toFixed(2))} ${units[exponent]}`;
};

const formatReset = (nextResetAt) => {
  if (!nextResetAt) return null;
  const target = new Date(nextResetAt).getTime();
  if (!Number.isFinite(target)) return null;
  const now = Date.now();
  const diffMs = target - now;
  if (diffMs <= 0) return '已可恢复';
  const totalMinutes = Math.floor(diffMs / 60000);
  const days = Math.floor(totalMinutes / (60 * 24));
  const hours = Math.floor((totalMinutes % (60 * 24)) / 60);
  const minutes = totalMinutes % 60;
  const parts = [];
  if (days > 0) parts.push(`${days} 天`);
  if (hours > 0) parts.push(`${hours} 小时`);
  if (parts.length === 0) parts.push(`${Math.max(1, minutes)} 分钟`);
  return parts.join(' ');
};

const QuotaAccountBar = ({ account }) => {
  if (!account || account.budget_bytes == null || account.remaining_bytes == null) {
    return null;
  }
  const budget = Math.max(0, Number(account.budget_bytes) || 0);
  const used = Math.max(0, Number(account.used_bytes) || 0);
  const reserved = Math.max(0, Number(account.active_reserved_bytes) || 0);
  const remaining = Math.max(0, Number(account.remaining_bytes) || 0);
  const consumed = used + reserved;
  const percent = budget > 0 ? Math.min(100, Math.round((consumed / budget) * 100)) : 0;
  const tone = percent >= 100 ? 'bg-red-500' : percent >= 80 ? 'bg-amber-500' : 'bg-emerald-500';
  const ariaLabel = `${account.remote_name || '账号'} 配额 ${formatBytes(used)} 已用, ${formatBytes(reserved)} 预留, 剩余 ${formatBytes(remaining)} 共 ${formatBytes(budget)}`;
  const resetText = formatReset(account.next_reset_at);
  const blocked = isFutureTimestamp(account.provider_blocked_until);
  return (
    <div className="min-w-0 max-w-full">
      <div className="flex items-center justify-between gap-1.5 text-xs text-gray-700">
        <span className="font-medium truncate min-w-0">{account.remote_name || `账号`}</span>
        <span className="shrink-0 text-gray-500">
          {formatBytes(used)} / {formatBytes(budget)}
        </span>
      </div>
      <div
        className="mt-1 h-1.5 w-full rounded-full bg-gray-200 overflow-hidden"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={budget}
        aria-valuenow={consumed}
        aria-label={ariaLabel}
      >
        <div
          className={`h-full ${tone} transition-all`}
          style={{ width: `${percent}%` }}
        />
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-gray-500">
        <span>剩余 {formatBytes(remaining)} · 预留 {formatBytes(reserved)}</span>
        {resetText && <span aria-live="polite">{resetText} 后恢复</span>}
        {blocked && <span className="text-red-700">Provider 阻断</span>}
      </div>
    </div>
  );
};

const isFutureTimestamp = (value) => {
  if (!value) return false;
  const t = new Date(value).getTime();
  return Number.isFinite(t) && t > Date.now();
};

const QuotaExhaustedNotice = ({ status }) => {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(id);
  }, []);
  if (!status) return null;
  const accounts = status.accounts || [];
  const allExhausted = status.all_accounts_exhausted === true;
  if (!allExhausted) return null;
  const resetAt = status.next_quota_reset_at;
  if (!resetAt) {
    return (
      <div className="mt-2 text-xs text-amber-700 bg-amber-50 border border-amber-200 rounded-md px-2.5 py-1.5" role="status">
        所有账号配额已用完，正在等待下一次窗口刷新后自动恢复。
      </div>
    );
  }
  const target = new Date(resetAt).getTime();
  if (!Number.isFinite(target)) {
    return null;
  }
  const diffMs = target - now;
  if (diffMs <= 0) {
    return (
      <div className="mt-2 text-xs text-emerald-700 bg-emerald-50 border border-emerald-200 rounded-md px-2.5 py-1.5" role="status">
        配额窗口已刷新，可恢复扫描。
      </div>
    );
  }
  const totalMinutes = Math.floor(diffMs / 60000);
  const days = Math.floor(totalMinutes / (60 * 24));
  const hours = Math.floor((totalMinutes % (60 * 24)) / 60);
  const minutes = totalMinutes % 60;
  const countdown = [days > 0 ? `${days} 天` : '', hours > 0 ? `${hours} 小时` : '', days === 0 && hours === 0 ? `${Math.max(1, minutes)} 分钟` : '']
    .filter(Boolean)
    .join(' ');
  const active = (status.batches || []).some((b) => b && b.process && b.process.active);
  return (
    <div
      className="mt-2 text-xs text-amber-900 bg-amber-50 border border-amber-200 rounded-md px-2.5 py-2 space-y-1"
      role="status"
      aria-live="polite"
    >
      <div className="font-medium">所有账号配额已用完</div>
      <div>
        {active ? '当前批次正在完成，完成后将自动暂停' : '已自动暂停扫描'}
        ，距配额窗口刷新 {countdown}。
      </div>
    </div>
  );
};

export { QuotaAccountBar, QuotaExhaustedNotice, formatBytes };
