import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  Ban,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  FileText,
  Info,
  Loader2,
  Plus,
  Play,
  RefreshCw,
  RotateCcw,
  Search,
  ShieldCheck,
  X,
} from 'lucide-react';
import {
  allocateManualRun,
  analyzeManualRun,
  cancelManualWorker,
  getManualWorker,
  getManualWorkerLogs,
  getManualWorkers,
  getManualRun,
  getManualRunAccounts,
  getManualRunFiles,
  getManualRuns,
  getManualAccounts,
  saveManualAccounts,
  retryManualWorker,
  startManualRun,
} from '../services/api';

const ACCOUNT_CAP_BYTES = 700 * 1000 * 1000 * 1000;
const RUN_CAP_BYTES = 2_400 * 1000 * 1000 * 1000;

const formatDecimalBytes = (value) => {
  const bytes = Number(value);
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1000)), units.length - 1);
  return `${parseFloat((bytes / (1000 ** exponent)).toFixed(2))} ${units[exponent]}`;
};

const formatDateTime = (value) => {
  if (!value) return '-';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
};

const accountIdOf = (account) => account?.account_id;
const accountNameOf = (account, index) => account?.remote_name || account?.name || `账号 ${index + 1}`;
const runIdOf = (payload) => payload?.run_id ?? payload?.id ?? payload?.run?.id ?? null;
const listOf = (payload, names) => {
  for (const name of names) {
    if (Array.isArray(payload?.[name])) return payload[name];
  }
  return [];
};

const statusOf = (run) => String(run?.status || run?.state || run?.phase || '').toLowerCase();
const analysisPending = (run) => ['analyzing', 'analysis_pending', 'queued'].includes(statusOf(run));
const allocationPending = (run) => ['allocating', 'allocation_pending'].includes(statusOf(run));
const analysisFailed = (run) => ['analysis_failed', 'failed'].includes(statusOf(run));
const allocationFailed = (run) => run?.allocation_failed === true || statusOf(run) === 'allocation_failed';
const analysisComplete = (run) => ['analyzed', 'analysis_complete', 'allocated', 'allocation_failed', 'preview', 'ready'].includes(statusOf(run));
const allocationComplete = (run) => ['allocated', 'preview', 'ready'].includes(statusOf(run)) || run?.allocation_status === 'allocated';
const executionRunState = (run) => ['running', 'succeeded', 'failed', 'cancelled', 'needs_attention'].includes(statusOf(run));
const isStaleRun = (run) => run?.stale === true || run?.analysis_stale === true || statusOf(run) === 'stale';

const createIdempotencyKey = (prefix) => `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;

const extractNextCursor = (payload) => payload?.next_cursor ?? payload?.nextCursor ?? payload?.pagination?.next_cursor ?? '';
const runFromResponse = (payload) => {
  if (!payload) return null;
  const run = payload.run || payload;
  return {
    ...run,
    ...(payload.status || {}),
    accounts: payload.accounts ?? run.accounts,
    accounts_next_cursor: payload.accounts_next_cursor ?? run.accounts_next_cursor,
    accounts_has_more: payload.accounts_has_more ?? run.accounts_has_more,
  };
};

const ManualAllocationWorkspace = ({ taskId, task }) => {
  const [availableAccounts, setAvailableAccounts] = useState([]);
  const [selectedIds, setSelectedIds] = useState([]);
  const [accountRevision, setAccountRevision] = useState(0);
  const [accountChoice, setAccountChoice] = useState('');
  const [accountIdInput, setAccountIdInput] = useState('');
  const [accountsLoading, setAccountsLoading] = useState(true);
  const [accountsSaving, setAccountsSaving] = useState(false);
  const [accountsError, setAccountsError] = useState('');
  const [accountsMessage, setAccountsMessage] = useState('');
  const [accountsConflict, setAccountsConflict] = useState(false);
  const [run, setRun] = useState(null);
  const [runLoading, setRunLoading] = useState(true);
  const [runError, setRunError] = useState('');
  const [actionState, setActionState] = useState({ kind: '', loading: false, error: '' });
  const [expandedGroups, setExpandedGroups] = useState({});
  const [accountGroups, setAccountGroups] = useState([]);
  const [accountCursor, setAccountCursor] = useState('');
  const [accountLoading, setAccountLoading] = useState(false);
  const [filePages, setFilePages] = useState({});
  const [fileSearch, setFileSearch] = useState('');
  const [fileFilter, setFileFilter] = useState('all');

  const loadAccounts = useCallback(async () => {
    setAccountsLoading(true);
    setAccountsError('');
    try {
      const response = await getManualAccounts(taskId);
      const payload = response.data || {};
      const listed = listOf(payload, ['accounts', 'items']);
      const selected = listed.map(accountIdOf);
      const merged = [...listed, ...listOf(payload, ['available_accounts', 'trusted_accounts'])];
      const unique = merged.filter((account, index, all) => {
        const accountId = accountIdOf(account);
        return accountId != null && all.findIndex(item => accountIdOf(item) === accountId) === index;
      });
      setAvailableAccounts(unique);
      setSelectedIds((selected.length ? selected : listed.map(accountIdOf)).filter(id => id != null));
      setAccountRevision(Number(payload.revision) || 0);
      setAccountsConflict(false);
    } catch (error) {
      setAccountsError(error.response?.data?.error || '可信账号列表暂时无法获取。');
    } finally {
      setAccountsLoading(false);
    }
  }, [taskId]);

  const loadLatestRun = useCallback(async () => {
    setRunLoading(true);
    setRunError('');
    try {
      const response = await getManualRuns(taskId);
      const payload = response.data || {};
      const runs = listOf(payload, ['runs', 'items']);
      const latest = payload.latest_run || payload.latest || runs[0] || null;
      if (runIdOf(latest)) {
        const detail = await getManualRun(runIdOf(latest));
        setRun(runFromResponse(detail.data) || latest);
      } else {
        setRun(null);
      }
    } catch (error) {
      setRunError(error.response?.data?.error || '手动分配运行记录暂时无法获取。');
    } finally {
      setRunLoading(false);
    }
  }, [taskId]);

  useEffect(() => {
    loadAccounts();
    loadLatestRun();
  }, [loadAccounts, loadLatestRun]);

  const runId = runIdOf(run);
  const runStatus = statusOf(run);
  const destinationPath = task.dest_type === 'remote' ? `${task.remote_name}:${task.remote_dir}` : task.remote_dir;
  const runConfigRevision = Number(run?.manual_config_revision);
  const loadedConfigRevision = Number(accountRevision);
  const configRevisionChanged = Boolean(run && Number.isFinite(runConfigRevision) && Number.isFinite(loadedConfigRevision) && runConfigRevision !== loadedConfigRevision);
  const inputsChanged = Boolean(run && (
    run.source_path !== task.source_dir ||
    run.destination_path !== destinationPath ||
    run.transfer_mode !== task.transfer_mode ||
    configRevisionChanged
  ));
  const needsReanalysis = run?.needs_explicit_reanalyze === true || isStaleRun(run) || inputsChanged;
  const stale = needsReanalysis;
  const selectedAccounts = useMemo(
    () => selectedIds.map(id => availableAccounts.find(account => accountIdOf(account) === id) || { account_id: id }),
    [availableAccounts, selectedIds],
  );
  const unusedAccounts = availableAccounts.filter(account => !selectedIds.includes(accountIdOf(account)));

  const refreshRun = useCallback(async () => {
    if (!runId) return;
    try {
      const response = await getManualRun(runId);
      setRun(runFromResponse(response.data));
      setRunError('');
    } catch (error) {
      setRunError(error.response?.data?.error || '运行状态刷新失败。');
    }
  }, [runId]);

  useEffect(() => {
    if (!runId || (!analysisPending(run) && !allocationPending(run))) return undefined;
    const timer = setInterval(refreshRun, 2000);
    return () => clearInterval(timer);
  }, [run, runId, refreshRun]);

  const updateSelection = (next) => {
    setSelectedIds(next);
    setAccountsMessage('');
  };

  const saveSelection = async () => {
    setAccountsSaving(true);
    setAccountsError('');
    setAccountsMessage('');
    try {
      const response = await saveManualAccounts(taskId, {
        account_ids: selectedIds.map(Number),
        expected_revision: accountRevision,
        idempotency_key: createIdempotencyKey('manual-accounts'),
      });
      setAccountRevision(Number(response.data?.revision) || accountRevision + 1);
      setAccountsMessage('账号顺序已保存。');
    } catch (error) {
      if (error.response?.status === 409) {
        await loadAccounts();
        setAccountsConflict(true);
        setAccountsMessage('账号配置已被其他操作更新，已重新加载最新顺序。请确认后再保存。');
      } else {
        setAccountsError(error.response?.data?.error || '账号顺序保存失败。');
      }
    } finally {
      setAccountsSaving(false);
    }
  };

  const startAnalyze = async () => {
    if (!task.source_dir || !task.remote_dir || selectedIds.length === 0) return;
    setActionState({ kind: 'analyze', loading: true, error: '' });
    setRunError('');
    try {
      const response = await analyzeManualRun(taskId, {
        source_path: task.source_dir,
        destination_path: destinationPath,
        transfer_mode: task.transfer_mode || 'copy',
        accounts: selectedIds.map(accountId => ({ account_id: Number(accountId) })),
        idempotency_key: createIdempotencyKey('manual-analyze'),
        ...(runId ? { expected_run_id: Number(runId), expected_revision: Number(run.revision) } : {}),
      });
      const nextRun = runFromResponse(response.data);
      setRun(nextRun);
      setAccountGroups([]);
      setFilePages({});
    } catch (error) {
      setActionState({ kind: 'analyze', loading: false, error: error.response?.data?.error || '分析启动失败。' });
      return;
    }
    setActionState({ kind: 'analyze', loading: false, error: '' });
  };

  const startAllocate = async () => {
    if (!runId || !analysisComplete(run) || needsReanalysis || allocationFailed(run) || allocationComplete(run)) return;
    setActionState({ kind: 'allocate', loading: true, error: '' });
    try {
      const response = await allocateManualRun(runId, {
        expected_run_id: Number(run.id || runId),
        expected_revision: run.expected_revision ?? run.revision,
        expected_config_revision: run.manual_config_revision ?? accountRevision,
        idempotency_key: createIdempotencyKey('manual-allocate'),
      });
      setRun(runFromResponse(response.data) || run);
      setAccountGroups([]);
      setFilePages({});
    } catch (error) {
      setActionState({ kind: 'allocate', loading: false, error: error.response?.data?.error || '分配生成失败，可能需要重新分析。' });
      await refreshRun();
      return;
    }
    setActionState({ kind: 'allocate', loading: false, error: '' });
  };

  const loadAccountGroups = useCallback(async (cursor = '') => {
    if (!runId) return;
    setAccountLoading(true);
    try {
      const response = await getManualRunAccounts(runId, cursor);
      const payload = response.data || {};
      const nextItems = listOf(payload, ['accounts', 'items']);
      setAccountGroups(previous => cursor ? [...previous, ...nextItems] : nextItems);
      setAccountCursor(extractNextCursor(payload));
    } catch (error) {
      setRunError(error.response?.data?.error || '预览账号分组加载失败。');
    } finally {
      setAccountLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    if (runId && (allocationComplete(run) || executionRunState(run))) loadAccountGroups();
  }, [runId, run, loadAccountGroups]);

  const loadFiles = async (key, options, cursor = '') => {
    if (!runId) return;
    setFilePages(previous => ({ ...previous, [key]: { ...(previous[key] || {}), loading: true, error: '' } }));
    try {
      const response = await getManualRunFiles(runId, { ...options, cursor });
      const payload = response.data || {};
      const items = listOf(payload, ['files', 'items']);
      setFilePages(previous => ({
        ...previous,
        [key]: {
          loading: false,
          error: '',
          items: cursor ? [...(previous[key]?.items || []), ...items] : items,
          nextCursor: extractNextCursor(payload),
        },
      }));
    } catch (error) {
      setFilePages(previous => ({ ...previous, [key]: { ...(previous[key] || {}), loading: false, error: error.response?.data?.error || '文件列表加载失败。' } }));
    }
  };

  const toggleGroup = (key, options) => {
    const open = !expandedGroups[key];
    setExpandedGroups(previous => ({ ...previous, [key]: open }));
    if (open && !filePages[key]) loadFiles(key, options);
  };

  const readyForAnalyze = selectedIds.length > 0 && Boolean(task.source_dir) && Boolean(task.remote_dir);
  const analysisState = stale ? 'stale' : analysisPending(run) ? 'analyzing' : analysisFailed(run) ? 'failed' : analysisComplete(run) ? 'complete' : 'idle';
  const allocationState = allocationPending(run) ? 'allocating' : allocationComplete(run) ? 'complete' : 'idle';
  const allocatedBytes = run?.assigned_bytes;
  const unassignedBytes = run?.unassigned_bytes;
  const unassignedCount = run?.unassigned_count;
  const executionState = ['running', 'succeeded', 'failed', 'cancelled', 'needs_attention'].includes(statusOf(run));
  const previewReady = !stale && (allocationState === 'complete' || executionState);

  return (
    <section className="bg-white rounded-xl shadow-sm border border-indigo-200 overflow-hidden" aria-labelledby="manual-allocation-heading">
      <div className="px-4 py-4 md:px-6 border-b border-indigo-100 bg-indigo-50/60 flex flex-col md:flex-row md:items-center justify-between gap-3">
        <div className="flex items-start gap-2">
          <ShieldCheck className="w-5 h-5 text-indigo-600 mt-0.5" />
          <div>
            <h2 id="manual-allocation-heading" className="font-semibold text-gray-900">手动分配</h2>
            <p className="text-xs text-gray-600 mt-0.5">选择 → 分析 → 分配 → 预览</p>
          </div>
        </div>
        <span className="text-xs font-medium text-indigo-800 bg-white border border-indigo-200 rounded-full px-2.5 py-1">{runStatus || '待分析'}</span>
      </div>

      <div className="p-4 md:p-6 space-y-5">
        <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
          <LimitMetric label="单账号上限" value={formatDecimalBytes(ACCOUNT_CAP_BYTES)} sub="十进制" />
          <LimitMetric label="单次运行上限" value={formatDecimalBytes(RUN_CAP_BYTES)} sub="十进制总容量" />
          <LimitMetric label="传输模式" value={task.transfer_mode || 'copy'} sub="由任务配置决定" />
        </div>

        <section className="border-t border-gray-200 pt-4" aria-labelledby="manual-accounts-heading">
          <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-3">
            <div>
              <h3 id="manual-accounts-heading" className="text-sm font-semibold text-gray-900">有序可信账号</h3>
              <p className="text-xs text-gray-500 mt-0.5">分配按此顺序填充账号；账号数量不固定。配置修订版 {accountRevision}。</p>
            </div>
            <button type="button" onClick={saveSelection} disabled={accountsSaving || accountsLoading} className="inline-flex min-h-11 w-full items-center justify-center gap-1.5 rounded-md border border-gray-300 px-2.5 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500 sm:w-auto sm:min-h-9">
              {accountsSaving ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <CheckCircle2 className="w-3.5 h-3.5" />}
              保存账号顺序
            </button>
          </div>
          {accountsLoading ? <InlineLoading text="正在获取可信账号..." /> : accountsError ? <InlineError message={accountsError} onRetry={loadAccounts} /> : (
            <>
              <div className="mt-3 space-y-2" role="list" aria-label="有序可信账号">
                {selectedAccounts.map((account, index) => (
                  <div key={accountIdOf(account) || index} className="flex items-center gap-2 border-b border-gray-100 py-2 last:border-b-0" role="listitem">
                    <span className="w-6 shrink-0 text-center text-xs font-semibold text-gray-500">{index + 1}</span>
                    <div className="min-w-0 flex-1"><div className="truncate text-sm font-medium text-gray-800">{accountNameOf(account, index)}</div><div className="text-[11px] text-gray-500">ID {accountIdOf(account) ?? '-'}</div></div>
                    <button type="button" onClick={() => updateSelection(selectedIds.filter(id => id !== accountIdOf(account)))} title="移除账号" aria-label={`移除${accountNameOf(account, index)}`} className="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-500 hover:bg-red-50 hover:text-red-600 focus:outline-none focus:ring-2 focus:ring-red-500"><X className="w-4 h-4" /></button>
                    <button type="button" disabled={index === 0} onClick={() => { const next = [...selectedIds]; [next[index - 1], next[index]] = [next[index], next[index - 1]]; updateSelection(next); }} title="上移" aria-label={`上移${accountNameOf(account, index)}`} className="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-500 hover:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-30 focus:outline-none focus:ring-2 focus:ring-indigo-500"><ArrowUp className="w-4 h-4" /></button>
                    <button type="button" disabled={index === selectedIds.length - 1} onClick={() => { const next = [...selectedIds]; [next[index], next[index + 1]] = [next[index + 1], next[index]]; updateSelection(next); }} title="下移" aria-label={`下移${accountNameOf(account, index)}`} className="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-500 hover:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-30 focus:outline-none focus:ring-2 focus:ring-indigo-500"><ArrowDown className="w-4 h-4" /></button>
                  </div>
                ))}
                {selectedAccounts.length === 0 && <div className="py-3 text-xs text-amber-700">请添加至少一个可信账号。</div>}
              </div>
              <div className="mt-3 grid grid-cols-1 sm:grid-cols-[minmax(0,1fr)_7rem_auto] gap-2">
                <select value={accountChoice} onChange={(event) => { setAccountChoice(event.target.value); setAccountIdInput(''); }} className="h-9 min-w-0 rounded-md border border-gray-300 px-2 text-xs text-gray-700 focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500" aria-label="选择要添加的可信账号">
                  <option value="">选择可信账号添加</option>
                  {unusedAccounts.map(account => <option key={accountIdOf(account)} value={accountIdOf(account)}>{accountNameOf(account)} · ID {accountIdOf(account)}</option>)}
                </select>
                <input type="number" min="1" value={accountIdInput} onChange={(event) => { setAccountIdInput(event.target.value); setAccountChoice(''); }} placeholder="账号 ID" className="h-9 min-w-0 rounded-md border border-gray-300 px-2 text-xs focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500" aria-label="输入可信账号 ID" />
                <button type="button" disabled={!Number(accountIdInput || accountChoice) || selectedIds.includes(Number(accountIdInput || accountChoice))} onClick={() => { const accountId = Number(accountIdInput || accountChoice); updateSelection([...selectedIds, accountId]); setAccountChoice(''); setAccountIdInput(''); }} className="inline-flex min-h-9 items-center justify-center gap-1 rounded-md border border-indigo-200 px-3 py-1.5 text-xs font-medium text-indigo-700 hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500"><Plus className="w-3.5 h-3.5" />添加账号</button>
              </div>
            </>
          )}
          {accountsMessage && <p className="mt-2 text-xs text-emerald-700" role="status">{accountsMessage}</p>}
          {accountsConflict && <StateNotice tone="warning" icon={AlertTriangle} text="账号配置发生并发变化。请确认重新加载的顺序后再保存。" />}
        </section>

        <section className="border-t border-gray-200 pt-4" aria-labelledby="manual-analysis-heading">
          <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
            <div className="min-w-0"><h3 id="manual-analysis-heading" className="text-sm font-semibold text-gray-900">分析源文件</h3><p className="text-xs text-gray-500 mt-0.5 break-words">{task.source_dir || '未设置源目录'} → {task.remote_name ? `${task.remote_name}:` : ''}{task.remote_dir || '未设置目标目录'}</p></div>
            <button type="button" onClick={startAnalyze} disabled={!readyForAnalyze || actionState.loading || analysisPending(run)} title={!readyForAnalyze ? '需要源目录、目标目录和至少一个账号' : '扫描文件但不上传或预留额度'} className="inline-flex min-h-11 w-full shrink-0 items-center justify-center gap-1.5 rounded-lg bg-indigo-600 px-3 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500 sm:w-auto sm:min-h-10">
              {actionState.kind === 'analyze' && actionState.loading ? <Loader2 className="w-4 h-4 animate-spin" /> : <RefreshCw className="w-4 h-4" />}
              {analysisComplete(run) || stale ? '重新分析' : '开始分析'}
            </button>
          </div>
          <div className="mt-3 min-h-[2.5rem]" aria-live="polite">
            {analysisPending(run) && <StateNotice tone="info" icon={Loader2} text="正在扫描文件；不会上传或预留额度。" spinning />}
            {analysisState === 'complete' && !stale && <StateNotice tone="success" icon={CheckCircle2} text={`分析完成 · ${run?.snapshot_count ?? '-'} 个文件 · ${formatDecimalBytes(run?.snapshot_bytes)}`} />}
            {analysisFailed(run) && <StateNotice tone="error" icon={AlertTriangle} text={run?.last_error || run?.error || '分析失败，请重新分析。'} />}
            {allocationFailed(run) && <StateNotice tone="error" icon={AlertTriangle} text={`分配失败：${run?.last_error || run?.error || '未知错误'}。请重新分析后再生成分配。`} />}
            {run?.needs_explicit_reanalyze === true && <StateNotice tone="warning" icon={AlertTriangle} text="当前运行需要显式重新分析；旧预览不能继续使用。" />}
            {stale && !run?.needs_explicit_reanalyze && <StateNotice tone="warning" icon={AlertTriangle} text="源、目标或账号顺序已变化，当前分析/预览已失效，请重新分析。" />}
            {actionState.error && <StateNotice tone="error" icon={AlertTriangle} text={actionState.error} />}
            {runError && <StateNotice tone="error" icon={AlertTriangle} text={runError} />}
            {!run && !runLoading && !actionState.error && <StateNotice tone="neutral" icon={Info} text="分析只读取文件树，完成后才能生成确定性分配预览。" />}
          </div>
        </section>

        {analysisComplete(run) && (
          <section className="border-t border-gray-200 pt-4" aria-labelledby="manual-allocation-heading">
            <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
              <div><h3 id="manual-allocation-heading" className="text-sm font-semibold text-gray-900">生成分配预览</h3><p className="text-xs text-gray-500 mt-0.5">按账号顺序分配完整文件，不拆分文件。</p></div>
              <button type="button" onClick={startAllocate} disabled={stale || allocationPending(run) || allocationFailed(run) || allocationComplete(run) || actionState.loading} title={stale || allocationFailed(run) ? '请重新分析后再生成分配' : allocationComplete(run) ? '当前分配已生成' : '生成分配预览，不启动传输'} className="inline-flex min-h-11 w-full shrink-0 items-center justify-center gap-1.5 rounded-lg border border-indigo-300 px-3 py-2 text-sm font-medium text-indigo-700 hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500 sm:w-auto sm:min-h-10">
                {allocationPending(run) || (actionState.kind === 'allocate' && actionState.loading) ? <Loader2 className="w-4 h-4 animate-spin" /> : <CheckCircle2 className="w-4 h-4" />}
                {allocationComplete(run) ? '已生成预览' : '生成预览'}
              </button>
            </div>
            {allocationPending(run) && <div className="mt-3"><StateNotice tone="info" icon={Loader2} text="正在生成确定性分配；不会启动传输。" spinning /></div>}
          </section>
        )}

        {previewReady && <ManualPreview run={run} onRunChange={setRun} accountRevision={accountRevision} accountGroups={accountGroups} accountCursor={accountCursor} accountLoading={accountLoading} loadAccountGroups={loadAccountGroups} expandedGroups={expandedGroups} toggleGroup={toggleGroup} filePages={filePages} loadFiles={loadFiles} fileSearch={fileSearch} setFileSearch={setFileSearch} fileFilter={fileFilter} setFileFilter={setFileFilter} allocatedBytes={allocatedBytes} unassignedBytes={unassignedBytes} unassignedCount={unassignedCount} />}
      </div>
    </section>
  );
};

const ManualPreview = ({ run, onRunChange, accountRevision, accountGroups, accountCursor, accountLoading, loadAccountGroups, expandedGroups, toggleGroup, filePages, loadFiles, fileSearch, setFileSearch, fileFilter, setFileFilter, allocatedBytes, unassignedBytes, unassignedCount }) => {
  const summary = run || {};
  const groups = accountGroups.length ? accountGroups : listOf(run, ['accounts', 'account_groups']);
  const visibleGroups = groups.filter(group => fileFilter === 'all' || fileFilter === 'assigned');
  return (
    <section className="border-t border-gray-200 pt-4" aria-labelledby="manual-preview-heading">
      <div className="flex flex-col md:flex-row md:items-start justify-between gap-3">
        <div><h3 id="manual-preview-heading" className="text-sm font-semibold text-gray-900">分配预览</h3><p className="text-xs text-gray-500 mt-0.5">修订版 {run.revision ?? run.expected_revision ?? '-'} · {formatDateTime(run.updated_at || run.created_at)} · 运行尚未启用</p></div>
        <div className="flex flex-col sm:flex-row gap-2"><label className="relative"><Search className="absolute left-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-gray-400" /><input value={fileSearch} onChange={event => setFileSearch(event.target.value)} placeholder="筛选文件名" className="h-9 w-full sm:w-44 rounded-md border border-gray-300 pl-7 pr-2 text-xs focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500" aria-label="筛选预览文件名" /></label><select value={fileFilter} onChange={event => setFileFilter(event.target.value)} className="h-9 rounded-md border border-gray-300 px-2 text-xs text-gray-700 focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500" aria-label="筛选预览分组"><option value="all">全部分组</option><option value="assigned">已分配</option><option value="oversize">文件过大</option><option value="aggregate_capacity">总容量不足</option><option value="account_capacity">账号容量不足</option></select></div>
      </div>

      <div className="mt-3 grid grid-cols-2 lg:grid-cols-5 gap-3">
        <LimitMetric label="已分配" value={formatDecimalBytes(allocatedBytes ?? summary.assigned_bytes)} sub={`${run.assigned_count ?? summary.assigned_file_count ?? '-'} 个文件`} />
        <LimitMetric label="未分配" value={formatDecimalBytes(unassignedBytes ?? summary.unassigned_bytes)} sub={`${unassignedCount ?? summary.unassigned_file_count ?? 0} 个文件`} />
        <LimitMetric label="未分配原因" value="按需查看" sub="文件过大 / 容量不足" />
        <LimitMetric label="总文件" value={run.snapshot_count ?? summary.total_file_count ?? '-'} sub="分析快照" />
        <LimitMetric label="运行账号" value={groups.length || '-'} sub="仅预览" />
      </div>

      <div className="mt-4 space-y-2">
        {visibleGroups.map((group, index) => {
          const id = accountIdOf(group) ?? index;
          return <ManualPreviewGroup key={id} group={group} groupKey={`account-${id}`} options={{ account_id: id }} expanded={expandedGroups[`account-${id}`]} toggleGroup={toggleGroup} filePage={filePages[`account-${id}`]} loadFiles={loadFiles} fileSearch={fileSearch} />;
        })}
        {(fileFilter === 'all' || fileFilter === 'oversize') && <ManualPreviewGroup group={{ name: '文件过大', file_count: run.oversize_count, bytes: run.oversize_bytes }} groupKey="oversize" options={{ reason: 'oversize' }} expanded={expandedGroups.oversize} toggleGroup={toggleGroup} filePage={filePages.oversize} loadFiles={loadFiles} fileSearch={fileSearch} tone="warning" />}
        {(fileFilter === 'all' || fileFilter === 'aggregate_capacity') && <ManualPreviewGroup group={{ name: '总容量不足', file_count: run.aggregate_capacity_count, bytes: run.aggregate_capacity_bytes }} groupKey="aggregate-capacity" options={{ reason: 'aggregate_capacity' }} expanded={expandedGroups['aggregate-capacity']} toggleGroup={toggleGroup} filePage={filePages['aggregate-capacity']} loadFiles={loadFiles} fileSearch={fileSearch} tone="warning" />}
        {(fileFilter === 'all' || fileFilter === 'account_capacity') && <ManualPreviewGroup group={{ name: '账号容量不足', file_count: run.account_capacity_count, bytes: run.account_capacity_bytes }} groupKey="account-capacity" options={{ reason: 'account_capacity' }} expanded={expandedGroups['account-capacity']} toggleGroup={toggleGroup} filePage={filePages['account-capacity']} loadFiles={loadFiles} fileSearch={fileSearch} tone="warning" />}
      </div>
      {accountCursor && <button type="button" onClick={() => loadAccountGroups(accountCursor)} disabled={accountLoading} className="mt-3 inline-flex min-h-9 items-center gap-1.5 rounded-md border border-gray-300 px-2.5 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500">{accountLoading && <Loader2 className="w-3.5 h-3.5 animate-spin" />}加载更多账号</button>}
      <ManualWorkerConsole run={run} onRunChange={onRunChange} accountRevision={accountRevision} />
    </section>
  );
};

const ManualPreviewGroup = ({ group, groupKey, options, expanded, toggleGroup, filePage, loadFiles, fileSearch, tone = 'default' }) => {
  const items = (filePage?.items || []).filter(file => !fileSearch || String(file.relative_path || '').toLowerCase().includes(fileSearch.toLowerCase()));
  const count = group.allocated_count ?? group.file_count ?? 0;
  const bytes = group.allocated_bytes ?? group.bytes;
  const usage = Math.max(0, Number(bytes) || 0);
  const usagePercent = Math.min(100, (usage / ACCOUNT_CAP_BYTES) * 100);
  const isAccountGroup = group.account_id != null;
  return (
    <div className={`border ${tone === 'warning' ? 'border-amber-200 bg-amber-50/50' : 'border-gray-200'} rounded-lg`}>
      <button type="button" onClick={() => toggleGroup(groupKey, options)} aria-expanded={expanded} className="w-full flex items-center gap-2 p-3 text-left hover:bg-white/70 focus:outline-none focus:ring-2 focus:ring-indigo-500">
        {expanded ? <ChevronDown className="w-4 h-4 text-gray-500" /> : <ChevronRight className="w-4 h-4 text-gray-500" />}
        <span className="min-w-0 flex-1"><span className="block truncate text-sm font-medium text-gray-800">{group.remote_name || group.name || `账号 ${group.account_id || ''}`}</span>{isAccountGroup && <span className="mt-1 block h-1.5 overflow-hidden rounded-full bg-gray-200" role="progressbar" aria-label={`${group.remote_name || `账号 ${group.account_id}`} 分配容量`} aria-valuemin={0} aria-valuemax={ACCOUNT_CAP_BYTES} aria-valuenow={usage}><span className="block h-full bg-indigo-500" style={{ width: `${usagePercent}%` }} /></span>}</span>
        <span className="shrink-0 text-right text-xs text-gray-500">{count} 个<br />{formatDecimalBytes(bytes)}</span>
      </button>
      {expanded && <div className="border-t border-gray-200 p-3"><div className="flex items-center gap-2 text-xs text-gray-500 mb-2"><FileText className="w-3.5 h-3.5" />文件列表按游标加载</div>{filePage?.loading && <InlineLoading text="正在加载文件..." />}{filePage?.error && <InlineError message={filePage.error} onRetry={() => loadFiles(groupKey, options)} />}{!filePage?.loading && !filePage?.error && items.length === 0 && <div className="py-3 text-xs text-gray-500">没有匹配的文件。</div>}<div className="max-h-64 overflow-auto space-y-1" role="list">{items.map((file, index) => <div key={file.id || file.relative_path || index} role="listitem" className="flex items-center justify-between gap-3 py-1.5 text-xs"><span className="min-w-0 truncate text-gray-700" title={file.relative_path}>{file.relative_path || '未命名文件'}</span><span className="shrink-0 text-gray-500">{formatDecimalBytes(file.size_bytes)}</span></div>)}</div>{filePage?.nextCursor && <button type="button" onClick={() => loadFiles(groupKey, options, filePage.nextCursor)} disabled={filePage.loading} className="mt-2 text-xs font-medium text-indigo-700 underline focus:outline-none focus:ring-2 focus:ring-indigo-500">加载更多</button>}</div>}
    </div>
  );
};

const workerIdOf = (worker) => worker?.worker_id ?? worker?.id;
const workerStatusOf = (worker) => String(worker?.state || worker?.status || '').toLowerCase();
const workerIsActive = (worker) => ['pending', 'starting', 'running', 'reconciling', 'cancel_requested'].includes(workerStatusOf(worker));
const workerIsTerminal = (worker) => ['succeeded', 'failed', 'cancelled', 'unknown', 'needs_attention'].includes(workerStatusOf(worker));
const workerCanCancel = (worker) => worker?.actionability === 'cancel';
const workerCanRetry = (worker) => worker?.actionability === 'retry';
const mergeWorkerData = (liveWorker, detail) => ({
  ...(detail?.worker || {}),
  ...liveWorker,
  attempts: detail?.attempts,
  files: detail?.files,
});
const workerLabel = (status) => ({
  pending: '待启动',
  starting: '启动中',
  running: '运行中',
  reconciling: '对账中',
  succeeded: '已完成',
  cancelled: '已取消',
  unknown: '状态未知',
  needs_attention: '需要人工处理',
  cancel_requested: '正在取消',
  failed: '失败',
}[status] || status || '未知');

const workersFromResponse = (payload) => {
  const body = payload?.workers ? payload : payload?.data || payload || {};
  return Array.isArray(body.workers) ? body.workers : Array.isArray(body.items) ? body.items : [];
};

const ManualWorkerConsole = ({ run, onRunChange, accountRevision }) => {
  const [workers, setWorkers] = useState([]);
  const [workersLoading, setWorkersLoading] = useState(true);
  const [workersError, setWorkersError] = useState('');
  const [startState, setStartState] = useState({ loading: false, error: '' });
  const [expanded, setExpanded] = useState({});
  const [details, setDetails] = useState({});
  const [logs, setLogs] = useState({});
  const [workerActions, setWorkerActions] = useState({});
  const runId = runIdOf(run);
  const isCopyRun = run?.transfer_mode === 'copy';
  const isAllocated = statusOf(run) === 'allocated' || run?.allocated === true;
  const succeededWorkers = workers.filter(worker => workerStatusOf(worker) === 'succeeded').length;
  const runDisplayStatus = statusOf(run) === 'failed' && succeededWorkers > 0 ? '部分完成 · 有失败' : statusOf(run) === 'cancelled' && succeededWorkers > 0 ? '部分完成 · 已取消剩余' : statusOf(run) === 'needs_attention' ? '需要人工处理' : statusOf(run) === 'succeeded' ? '已完成' : statusOf(run) === 'failed' ? '失败' : statusOf(run) === 'cancelled' ? '已取消' : statusOf(run) || '待启动';

  const loadWorkers = useCallback(async () => {
    if (!runId) return;
    setWorkersLoading(true);
    try {
      const response = await getManualWorkers(runId);
      setWorkers(workersFromResponse(response.data));
      setWorkersError('');
    } catch (error) {
      setWorkersError(error.response?.data?.error || 'Worker 状态暂时无法获取。');
    } finally {
      setWorkersLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    setWorkers([]);
    setDetails({});
    setLogs({});
    setExpanded({});
    loadWorkers();
  }, [runId, loadWorkers]);

  const activeWorkers = workers.some(workerIsActive);
  const runTerminal = ['succeeded', 'failed', 'cancelled', 'needs_attention'].includes(statusOf(run));
  const runNeedsPolling = workers.length > 0 && (!runTerminal || activeWorkers);
  const pollExecutionState = useCallback(async () => {
    await loadWorkers();
    if (!runId) return;
    try {
      const response = await getManualRun(runId);
      onRunChange(runFromResponse(response.data) || run);
    } catch (error) {
      setWorkersError(error.response?.data?.error || '运行状态刷新失败。');
    }
  }, [loadWorkers, onRunChange, runId, run]);

  useEffect(() => {
    if (!runId || (!runNeedsPolling && !startState.loading)) return undefined;
    const timer = setInterval(pollExecutionState, 2000);
    return () => clearInterval(timer);
  }, [runId, runNeedsPolling, startState.loading, pollExecutionState]);

  const loadWorkerDetail = async (workerId) => {
    try {
      const response = await getManualWorker(workerId);
      setDetails(previous => ({ ...previous, [workerId]: response.data }));
    } catch (error) {
      setWorkersError(error.response?.data?.error || 'Worker 详情加载失败。');
    }
  };

  const loadWorkerLogs = useCallback(async (workerId) => {
    const current = logs[workerId] || { nextOffset: 0, items: [], eof: false, epoch: 0 };
    const liveWorker = workers.find(worker => workerIdOf(worker) === workerId);
    if (current.eof) return;
    const requestEpoch = current.epoch || 0;
    setLogs(previous => {
      const latest = previous[workerId] || current;
      if ((latest.epoch || 0) !== requestEpoch) return previous;
      return { ...previous, [workerId]: { ...latest, loading: true, error: '' } };
    });
    try {
      const response = await getManualWorkerLogs(workerId, current.nextOffset, 200);
      const payload = response.data || {};
      const newItems = typeof payload.data === 'string'
        ? payload.data.split(/\r?\n/).filter(Boolean).map(text => ({ text }))
        : Array.isArray(payload.events) ? payload.events : [];
      const nextOffset = Number(payload.next_offset ?? current.nextOffset);
      const eof = workerIsTerminal(liveWorker) && payload.eof === true;
      setLogs(previous => {
        const latest = previous[workerId] || current;
        if ((latest.epoch || 0) !== requestEpoch) return previous;
        return {
          ...previous,
          [workerId]: { ...latest, items: [...(latest.items || []), ...newItems], nextOffset, eof, loading: false, error: '' },
        };
      });
    } catch (error) {
      setLogs(previous => {
        const latest = previous[workerId] || current;
        if ((latest.epoch || 0) !== requestEpoch) return previous;
        return { ...previous, [workerId]: { ...latest, loading: false, error: error.response?.data?.error || 'Worker 日志加载失败。' } };
      });
    }
  }, [logs, workers]);

  useEffect(() => {
    const expandedNeedingLogs = workers.filter(worker => expanded[workerIdOf(worker)] && !logs[workerIdOf(worker)]?.eof);
    if (expandedNeedingLogs.length === 0) return undefined;
    const timer = setInterval(() => expandedNeedingLogs.forEach(worker => loadWorkerLogs(workerIdOf(worker))), 2000);
    return () => clearInterval(timer);
  }, [workers, expanded, logs, loadWorkerLogs]);

  const toggleWorker = (worker) => {
    const workerId = workerIdOf(worker);
    const nextOpen = !expanded[workerId];
    setExpanded(previous => ({ ...previous, [workerId]: nextOpen }));
    if (nextOpen) {
      loadWorkerDetail(workerId);
      if (!logs[workerId]) loadWorkerLogs(workerId);
    }
  };

  const startWorkers = async () => {
    if (!runId || !isAllocated || !isCopyRun || startState.loading) return;
    if (Number(run.unassigned_count) > 0 && !window.confirm(`仍有 ${run.unassigned_count} 个文件未分配。只启动已分配账号的 worker？`)) return;
    setStartState({ loading: true, error: '' });
    try {
      const response = await startManualRun(runId, {
        expected_run_id: Number(run.id || runId),
        expected_revision: Number(run.revision),
        expected_config_revision: Number(run.manual_config_revision ?? accountRevision),
        idempotency_key: createIdempotencyKey('manual-start'),
      });
      onRunChange(runFromResponse(response.data) || run);
      await loadWorkers();
    } catch (error) {
      setStartState({ loading: false, error: error.response?.data?.error || '启动 worker 失败。请刷新状态后重试。' });
      return;
    }
    setStartState({ loading: false, error: '' });
  };

  const workerAction = async (worker, action) => {
    const workerId = workerIdOf(worker);
    setWorkerActions(previous => ({ ...previous, [workerId]: action }));
    try {
      if (action === 'cancel') await cancelManualWorker(workerId);
      else {
        await retryManualWorker(workerId);
        setLogs(previous => {
          const current = previous[workerId] || { nextOffset: 0, items: [], eof: false, epoch: 0 };
          return { ...previous, [workerId]: { ...current, epoch: (current.epoch || 0) + 1, eof: false, loading: false, error: '' } };
        });
      }
      await loadWorkers();
    } catch (error) {
      setWorkersError(error.response?.data?.error || `${action === 'cancel' ? '取消' : '重试'} worker 失败。`);
    } finally {
      setWorkerActions(previous => ({ ...previous, [workerId]: '' }));
    }
  };

  return (
    <section className="mt-4 border-t border-gray-200 pt-4" aria-labelledby="manual-workers-heading">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div><h3 id="manual-workers-heading" className="text-sm font-semibold text-gray-900">Worker 控制台</h3><p className="text-xs text-gray-500 mt-0.5">运行状态：{runDisplayStatus} · 每个已分配账号一个独立 worker；不会自动续跑。</p></div>
        {isCopyRun ? <button type="button" onClick={startWorkers} disabled={!isAllocated || startState.loading} title={!isAllocated ? '只有已分配的运行才能启动' : '显式启动已分配账号的 worker'} className="inline-flex min-h-11 w-full sm:w-auto items-center justify-center gap-1.5 rounded-lg bg-indigo-600 px-3 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500">{startState.loading ? <Loader2 className="w-4 h-4 animate-spin" /> : <Play className="w-4 h-4" />}开始运行</button> : <span className="w-full sm:w-auto text-center text-xs font-medium text-amber-800 bg-amber-50 border border-amber-200 rounded-lg px-2.5 py-2">移动模式暂不可运行</span>}
      </div>
      <div className="mt-2 min-h-[1.25rem]" aria-live="polite">{startState.error && <StateNotice tone="error" icon={AlertTriangle} text={startState.error} />}{workersError && <StateNotice tone="error" icon={AlertTriangle} text={workersError} />}</div>
      {workersLoading && workers.length === 0 ? <InlineLoading text="正在获取 worker 状态..." /> : workers.length === 0 ? <div className="py-3 text-xs text-gray-500">尚未创建 worker。开始运行后，已分配账号会独立显示。</div> : <div className="mt-2 space-y-2" role="list" aria-label="手动传输 worker 列表">{workers.map(worker => <ManualWorkerRow key={workerIdOf(worker)} worker={mergeWorkerData(worker, details[workerIdOf(worker)])} expanded={expanded[workerIdOf(worker)]} onToggle={() => toggleWorker(worker)} onAction={workerAction} onRetryLogs={() => loadWorkerLogs(workerIdOf(worker))} action={workerActions[workerIdOf(worker)]} logs={logs[workerIdOf(worker)]} />)}</div>}
    </section>
  );
};

const ManualWorkerRow = ({ worker, expanded, onToggle, onAction, onRetryLogs, action, logs }) => {
  const status = workerStatusOf(worker);
  const workerId = workerIdOf(worker);
  const completedBytes = Number(worker.completed_bytes || 0);
  const totalBytes = Number(worker.assigned_bytes || 0);
  const percent = Number.isFinite(Number(worker.progress_percent)) ? Number(worker.progress_percent) : totalBytes > 0 ? Math.min(100, (completedBytes / totalBytes) * 100) : 0;
  const currentFile = worker.current_relative_path || '-';
  const speed = worker.speed_bytes_per_second;
  const needsAttention = status === 'unknown' || status === 'needs_attention';
  return (
    <div className="border border-gray-200 rounded-lg" role="listitem">
      <div className="flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:gap-3">
        <button type="button" onClick={onToggle} aria-expanded={expanded} aria-controls={`worker-detail-${workerId}`} className="flex min-w-0 flex-1 items-start gap-2 rounded text-left focus:outline-none focus:ring-2 focus:ring-indigo-500"><span className="mt-0.5">{expanded ? <ChevronDown className="w-4 h-4 text-gray-500" /> : <ChevronRight className="w-4 h-4 text-gray-500" />}</span><span className="min-w-0 flex-1"><span className="flex flex-wrap items-center gap-2"><span className="min-w-0 break-words text-sm font-medium text-gray-800">{worker.remote_name || `账号 ${worker.account_id ?? workerId}`}</span><StatusPill status={status} /><span className="text-[11px] font-medium text-indigo-600">{expanded ? '收起日志' : '查看日志'}</span></span><span className="mt-1 block break-words text-xs text-gray-500">{currentFile}</span>{needsAttention && <span className="mt-1 block text-[11px] text-amber-800">需要查看日志并人工处理</span>}<span className="mt-1 block h-1.5 overflow-hidden rounded-full bg-gray-200" role="progressbar" aria-label="Worker 进度" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}><span className="block h-full bg-indigo-500 transition-all" style={{ width: `${percent}%` }} /></span></span></button>
        <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-left text-[11px] text-gray-500 sm:w-52 sm:shrink-0 sm:text-right"><span>文件 {worker.completed_count || 0}/{worker.assigned_count || 0}</span><span>尝试 {worker.attempt_number || 1}</span><span>{formatDecimalBytes(completedBytes)} / {formatDecimalBytes(totalBytes)}</span><span>{speed == null ? '-' : `${formatDecimalBytes(speed)}/s`}</span></div>
        <div className="flex w-full shrink-0 gap-2 border-t border-gray-100 pt-2 sm:w-20 sm:border-0 sm:pt-0 sm:justify-end">{workerCanCancel(worker) && <button type="button" onClick={() => onAction(worker, 'cancel')} disabled={Boolean(action)} title="取消 worker" aria-label="取消 worker" className="inline-flex min-h-11 min-w-11 flex-1 items-center justify-center rounded-md border border-red-200 text-red-700 hover:bg-red-50 disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-red-500 sm:flex-none sm:h-9 sm:min-h-0 sm:w-9">{action === 'cancel' ? <Loader2 className="w-4 h-4 animate-spin" /> : <Ban className="w-4 h-4" />}</button>}{workerCanRetry(worker) && <button type="button" onClick={() => onAction(worker, 'retry')} disabled={Boolean(action)} title="重试 worker" aria-label="重试 worker" className="inline-flex min-h-11 min-w-11 flex-1 items-center justify-center rounded-md border border-indigo-200 text-indigo-700 hover:bg-indigo-50 disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-indigo-500 sm:flex-none sm:h-9 sm:min-h-0 sm:w-9">{action === 'retry' ? <Loader2 className="w-4 h-4 animate-spin" /> : <RotateCcw className="w-4 h-4" />}</button>}</div>
      </div>
      {expanded && <div id={`worker-detail-${workerId}`} className="border-t border-gray-200 p-3"><div className="flex min-w-0 flex-wrap items-center justify-between gap-2"><h4 className="min-w-0 max-w-full break-words text-xs font-semibold text-gray-800">Worker 日志</h4><span className="min-w-0 max-w-full break-words text-[11px] text-gray-500">仅显示此 worker 的增量事件</span></div>{needsAttention && <StateNotice tone="warning" icon={AlertTriangle} text={worker.last_error || 'Worker 启动或对账状态需要人工确认。请查看此 worker 日志；后端允许时可重试。'} />}{worker.last_error && !needsAttention && <StateNotice tone="error" icon={AlertTriangle} text={worker.last_error} />}{logs?.loading && <InlineLoading text="正在加载日志..." />}{logs?.error && <InlineError message={logs.error} onRetry={onRetryLogs} />}{!logs?.loading && !logs?.error && logs?.items?.length === 0 && <div className="py-3 text-xs text-gray-500">暂无日志事件。</div>}{logs?.eof && <div className="mt-2 text-[11px] text-gray-500">日志已读取至末尾。</div>}<div className="mt-2 max-h-[min(50vh,24rem)] overflow-auto rounded-md bg-gray-50 p-2 font-mono text-[11px] leading-5 text-gray-700 sm:text-xs" role="log" aria-live="polite">{(logs?.items || []).map((entry, index) => <div key={entry.id || `${entry.offset || index}-${index}`} className="break-all">{entry.text}</div>)}</div></div>}
    </div>
  );
};

const StatusPill = ({ status }) => <span className={`shrink-0 rounded-full px-2 py-0.5 text-[10px] font-medium ${status === 'succeeded' ? 'bg-emerald-100 text-emerald-800' : status === 'failed' ? 'bg-red-100 text-red-800' : status === 'cancelled' ? 'bg-gray-100 text-gray-700' : status === 'unknown' || status === 'needs_attention' ? 'bg-amber-100 text-amber-800' : 'bg-indigo-100 text-indigo-800'}`}>{workerLabel(status)}</span>;

const LimitMetric = ({ label, value, sub }) => <div className="border border-gray-200 rounded-lg p-3"><div className="text-xs text-gray-500">{label}</div><div className="mt-1 text-lg font-semibold text-gray-900">{value}</div><div className="mt-0.5 text-xs text-gray-500">{sub}</div></div>;
const InlineLoading = ({ text }) => <div className="flex items-center gap-2 py-2 text-xs text-gray-500"><Loader2 className="w-3.5 h-3.5 animate-spin" />{text}</div>;
const InlineError = ({ message, onRetry }) => <div className="flex flex-wrap items-center justify-between gap-2 py-2 text-xs text-red-700" role="alert"><span>{message}</span><button type="button" onClick={onRetry} className="font-medium underline focus:outline-none focus:ring-2 focus:ring-red-500">重试</button></div>;
const StateNotice = ({ tone, icon: Icon, text, spinning }) => <div className={`flex items-center gap-2 text-xs ${tone === 'error' ? 'text-red-700' : tone === 'success' ? 'text-emerald-700' : tone === 'warning' ? 'text-amber-800' : 'text-gray-600'}`} role={tone === 'error' ? 'alert' : 'status'}><Icon className={`w-3.5 h-3.5 shrink-0 ${spinning ? 'animate-spin' : ''}`} />{text}</div>;

export default ManualAllocationWorkspace;
