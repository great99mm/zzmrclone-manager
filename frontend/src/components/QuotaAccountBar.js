import React, { useEffect, useState } from 'react';
import { Check, RotateCcw, X } from 'lucide-react';

const formatBytes = (bytes) => {
  if (bytes == null || bytes <= 0) {
    return '0 B';
  }
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1000)), units.length - 1);
  return `${parseFloat((bytes / (1000 ** exponent)).toFixed(2))} ${units[exponent]}`;
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

const formatDateTime = (value) => {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
};

const QuotaAccountBar = ({ account, taskId, onManualReset }) => {
  const [resetOpen, setResetOpen] = useState(false);
  const [confirmation, setConfirmation] = useState('');
  const [resetState, setResetState] = useState({ status: 'idle', message: '' });
  if (!account || account.budget_bytes == null || account.remaining_bytes == null) {
    return null;
  }
  const budget = Math.max(0, Number(account.budget_bytes) || 0);
  const uploaded = Math.max(0, Number(account.rolling_usage_bytes ?? account.used_bytes) || 0);
  const uploading = Math.max(0, Number(account.uploading_bytes) || 0);
  const queued = Math.max(0, (Number(account.active_reserved_bytes) || 0) - uploading);
  const remaining = Math.max(0, Number(account.remaining_bytes) || 0);
  const unresolved = Math.max(0, Number(account.unresolved_bytes) || 0);
  const consumed = uploaded + uploading + queued;
  const availabilityState = account.availability_state;
  const providerBlocked = availabilityState === 'provider_blocked' || isFutureTimestamp(account.provider_blocked_until);
  const campaignCooldown = availabilityState === 'campaign_cooldown';
  const blocked = providerBlocked;
  const displayedConsumed = blocked ? budget : consumed;
  const displayedRemaining = blocked ? 0 : remaining;
  const uploadedPct = budget > 0 ? Math.min(100, (uploaded / budget) * 100) : 0;
  const uploadingPct = budget > 0 ? Math.min(100, (uploading / budget) * 100) : 0;
  const queuedPct = budget > 0 ? Math.min(100, (queued / budget) * 100) : 0;
  const blockedPct = blocked ? Math.max(0, 100 - Math.min(100, uploadedPct + uploadingPct + queuedPct)) : 0;
  const ariaLabel = `${account.remote_name || '账号'} 已上传 ${formatBytes(uploaded)}, 传输中 ${formatBytes(uploading)}, 等待 ${formatBytes(queued)}, ${blocked ? '额度已满' : `剩余 ${formatBytes(displayedRemaining)}`}`;
  const recoveryAt = account.next_recovery_at || (campaignCooldown ? account.cooldown_until : null) || (providerBlocked ? account.provider_blocked_until : null);
  const recoveryLabel = campaignCooldown ? '滚动额度恢复' : providerBlocked ? 'Provider 阻断解除' : '预计可用';
  const accountId = account.account_id;
  const canReset = taskId && onManualReset && typeof accountId === 'number' && Number.isFinite(accountId);

  const submitReset = async () => {
    if (confirmation !== 'RESET' || resetState.status === 'pending') return;
    setResetState({ status: 'pending', message: '' });
    try {
      await onManualReset(accountId);
      setResetState({ status: 'success', message: '本地历史与活动冷却已清除，正在刷新状态。进行中使用和 Google/Provider 阻断保持不变。' });
      setConfirmation('');
      setResetOpen(false);
    } catch (error) {
      setResetState({ status: 'error', message: error.response?.data?.error || '清除失败，请稍后重试。' });
    }
  };
  return (
    <div className="min-w-0 max-w-full">
      <div className="flex items-center justify-between gap-1.5 text-xs text-gray-700">
        <div className="flex min-w-0 flex-wrap items-center gap-1.5">
          <span className="font-medium truncate min-w-0">{account.remote_name || `账号`}</span>
          {account.primary === true && <span className="shrink-0 rounded-full bg-amber-50 px-1.5 py-0.5 text-[10px] text-amber-700">主账号</span>}
        </div>
        <span className="shrink-0 text-gray-500">
          {blocked
            ? `Provider 阻断 / ${formatBytes(budget)}`
            : `已留 ${formatBytes(displayedConsumed)} / ${formatBytes(budget)}`}
        </span>
      </div>
      <div
        className="mt-1 h-1.5 w-full rounded-full bg-gray-200 overflow-hidden flex"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={budget}
        aria-valuenow={displayedConsumed}
        aria-label={ariaLabel}
      >
        <div className="h-full bg-emerald-500 transition-all" style={{ width: `${uploadedPct}%` }} />
        <div className="h-full bg-blue-500 transition-all" style={{ width: `${uploadingPct}%` }} />
        <div className="h-full bg-amber-400 transition-all" style={{ width: `${queuedPct}%` }} />
        <div className="h-full bg-red-500 transition-all" style={{ width: `${blockedPct}%` }} />
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-gray-500">
        {uploaded > 0 && <span className="text-emerald-700">已上传 {formatBytes(uploaded)}</span>}
        {uploading > 0 && <span className="text-blue-600">传输中 {formatBytes(uploading)}</span>}
        {queued > 0 && <span className="text-amber-700">等待 {formatBytes(queued)}</span>}
        {account.rolling_usage_bytes != null && <span>滚动已用 {formatBytes(uploaded)}</span>}
        {unresolved > 0 && <span className="text-amber-700">待确认 {formatBytes(unresolved)}</span>}
        <span>{blocked ? 'Provider 403 阻断' : `剩余 ${formatBytes(displayedRemaining)}`}</span>
        {recoveryAt && <span aria-live="polite">{recoveryLabel}：{formatReset(recoveryAt)} 后（{formatDateTime(recoveryAt)}）</span>}
        {campaignCooldown && account.cooldown_until && <span>24小时活动冷却至 {formatDateTime(account.cooldown_until)}</span>}
        {providerBlocked && <span className="text-red-700">保留 Google/Provider 阻断</span>}
      </div>
      <div className="mt-1 min-h-[2.25rem] flex items-center justify-end">
        {canReset && resetState.status !== 'success' && !resetOpen && (
          <button type="button" onClick={() => setResetOpen(true)} title="清除本地额度历史记录" aria-label="清除本地额度历史记录" className="inline-flex min-h-9 items-center gap-1.5 rounded-md border border-red-200 px-2.5 py-1.5 text-[11px] font-medium text-red-700 hover:bg-red-50 focus:outline-none focus:ring-2 focus:ring-red-500">
            <RotateCcw className="h-3.5 w-3.5" />
            清除本地记录
          </button>
        )}
        {canReset && resetOpen && resetState.status !== 'pending' && (
          <div className="flex w-full flex-col gap-2 sm:flex-row sm:items-center sm:justify-end" role="group" aria-label="确认清除本地额度记录">
            <span className="text-[11px] text-red-700 sm:mr-auto">输入 RESET：清除本地历史与活动冷却；保留进行中使用和 Google/Provider 阻断</span>
            <input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} className="h-9 w-28 rounded-md border border-red-200 px-2 text-xs uppercase focus:border-red-500 focus:outline-none focus:ring-2 focus:ring-red-500" aria-label="输入 RESET 确认" autoFocus />
            <button type="button" onClick={submitReset} disabled={confirmation !== 'RESET'} title="确认清除本地记录" aria-label="确认清除本地记录" className="inline-flex h-9 items-center justify-center rounded-md bg-red-600 px-2.5 text-xs font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-red-500"><Check className="h-3.5 w-3.5" /></button>
            <button type="button" onClick={() => { setResetOpen(false); setConfirmation(''); }} title="取消清除" aria-label="取消清除" className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-gray-300 text-gray-600 hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-gray-500"><X className="h-3.5 w-3.5" /></button>
          </div>
        )}
        {canReset && resetState.status === 'pending' && <span className="text-[11px] text-gray-500" aria-live="polite">正在清除本地记录...</span>}
        {canReset && resetState.status === 'success' && <span className="inline-flex items-center gap-1 text-[11px] text-emerald-700" role="status"><Check className="h-3.5 w-3.5" />{resetState.message}</span>}
        {canReset && resetState.status === 'error' && <div className="flex w-full items-center justify-end gap-2"><span className="text-[11px] text-red-700" role="alert">{resetState.message}</span><button type="button" onClick={() => setResetState({ status: 'idle', message: '' })} className="text-[11px] font-medium text-red-700 underline focus:outline-none focus:ring-2 focus:ring-red-500">重试</button></div>}
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
  const campaignAccounts = accounts.filter(account => account.availability_state === 'campaign_cooldown');
  const providerAccounts = accounts.filter(account => account.availability_state === 'provider_blocked');
  const recoveryTimes = accounts
    .map(account => account.next_recovery_at || (account.availability_state === 'campaign_cooldown' ? account.cooldown_until : null))
    .filter(Boolean)
    .map(value => ({ value, time: new Date(value).getTime() }))
    .filter(item => Number.isFinite(item.time) && item.time > now)
    .sort((a, b) => a.time - b.time);
  const resetAt = status.next_recovery_at || recoveryTimes[0]?.value;
  if (!resetAt) {
    return (
      <div className="mt-2 text-xs text-amber-700 bg-amber-50 border border-amber-200 rounded-md px-2.5 py-1.5" role="status">
        {campaignAccounts.length > 0
          ? '本地额度已达到上限，正在等待 24 小时滚动冷却结束。'
          : providerAccounts.length > 0
            ? '所有可用账号当前受到 Google/Provider 阻断，等待提供方解除。'
            : '所有可用账号暂不可用，等待状态恢复。'}
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
        滚动额度已恢复，可继续扫描。
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
      <div className="font-medium">{campaignAccounts.length > 0 ? '本地额度冷却中' : 'Provider 阻断恢复中'}</div>
      <div>
        {active ? '当前批次正在完成，完成后将自动暂停' : '已自动暂停扫描'}
        ，距最早可用时间 {countdown}。
      </div>
    </div>
  );
};

export { QuotaAccountBar, QuotaExhaustedNotice, formatBytes };
