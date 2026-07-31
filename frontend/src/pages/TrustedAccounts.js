import React, { useEffect, useState } from 'react';
import { Plus, RefreshCw, Save, ShieldCheck } from 'lucide-react';
import toast from 'react-hot-toast';
import { createQuotaAccount, getQuotaAccounts, getRemotes, updateQuotaAccount } from '../services/api';

const defaultAccount = { remote_name: '', budget_gb: '700', window_hours: '24' };

const bytesToGigabytes = (value) => (Number(value) / 1_000_000_000).toString();
const secondsToHours = (value) => (Number(value) / 3600).toString();

const TrustedAccounts = () => {
  const [accounts, setAccounts] = useState([]);
  const [remotes, setRemotes] = useState([]);
  const [draft, setDraft] = useState(defaultAccount);
  const [edits, setEdits] = useState({});
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [savingID, setSavingID] = useState(null);

  const loadData = async () => {
    setLoading(true);
    try {
      const [accountsResponse, remotesResponse] = await Promise.all([getQuotaAccounts(), getRemotes()]);
      setAccounts(accountsResponse.data.accounts || []);
      setRemotes(remotesResponse.data.remotes || []);
    } catch (error) {
      toast.error(error.response?.data?.error || '账号配置加载失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadData();
  }, []);

  const accountDraft = (account) => edits[account.id] || {
    budget_gb: bytesToGigabytes(account.budget_bytes),
    window_hours: secondsToHours(account.window_seconds),
    enabled: account.enabled,
  };

  const updateDraft = (account, changes) => {
    setEdits((current) => ({ ...current, [account.id]: { ...accountDraft(account), ...changes } }));
  };

  const parseQuota = (budgetGB, windowHours) => {
    const budget = Number(budgetGB);
    const window = Number(windowHours);
    const budgetBytes = Math.round(budget * 1_000_000_000);
    const windowSeconds = Math.round(window * 3600);
    if (!Number.isSafeInteger(budgetBytes) || budgetBytes <= 0 || !Number.isSafeInteger(windowSeconds) || windowSeconds <= 0) {
      throw new Error('请输入大于 0 的额度和周期');
    }
    return { budget_bytes: budgetBytes, window_seconds: windowSeconds };
  };

  const createAccount = async (event) => {
    event.preventDefault();
    if (!draft.remote_name) {
      toast.error('请选择远程盘');
      return;
    }
    let quota;
    try {
      quota = parseQuota(draft.budget_gb, draft.window_hours);
    } catch (error) {
      toast.error(error.message);
      return;
    }
    setCreating(true);
    try {
      const response = await createQuotaAccount({ remote_name: draft.remote_name, ...quota });
      setAccounts((current) => [...current, response.data].sort((a, b) => a.remote_name.localeCompare(b.remote_name)));
      setDraft(defaultAccount);
      toast.success('可信账号已添加');
    } catch (error) {
      toast.error(error.response?.data?.error || '添加可信账号失败');
    } finally {
      setCreating(false);
    }
  };

  const saveAccount = async (account) => {
    const current = accountDraft(account);
    let quota;
    try {
      quota = parseQuota(current.budget_gb, current.window_hours);
    } catch (error) {
      toast.error(error.message);
      return;
    }
    setSavingID(account.id);
    try {
      const response = await updateQuotaAccount(account.id, { ...quota, enabled: current.enabled });
      setAccounts((items) => items.map((item) => item.id === account.id ? response.data : item));
      setEdits((items) => {
        const next = { ...items };
        delete next[account.id];
        return next;
      });
      toast.success('账号配置已保存');
    } catch (error) {
      toast.error(error.response?.data?.error || '保存账号配置失败');
    } finally {
      setSavingID(null);
    }
  };

  const configuredRemotes = remotes.filter((remote) => !accounts.some((account) => account.remote_name === remote));

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">账号管理</h1>
          <p className="mt-1 text-gray-500">可信账号</p>
        </div>
        <button type="button" onClick={loadData} disabled={loading} title="刷新账号列表" aria-label="刷新账号列表" className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-gray-300 text-gray-600 hover:bg-gray-50 disabled:opacity-50">
          <RefreshCw className={`h-4 w-4 ${loading ? 'animate-spin' : ''}`} />
        </button>
      </div>

      <form onSubmit={createAccount} className="grid gap-3 rounded-lg border border-gray-200 bg-white p-4 shadow-sm md:grid-cols-[minmax(0,1fr)_150px_150px_auto] md:items-end">
        <label className="min-w-0 text-sm font-medium text-gray-700">
          远程盘
          <select value={draft.remote_name} onChange={(event) => setDraft((current) => ({ ...current, remote_name: event.target.value }))} disabled={loading || configuredRemotes.length === 0} className="mt-1 h-10 w-full rounded-md border border-gray-300 bg-white px-3 text-sm text-gray-900 focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:bg-gray-100">
            <option value="">选择远程盘</option>
            {configuredRemotes.map((remote) => <option key={remote} value={remote}>{remote}</option>)}
          </select>
        </label>
        <label className="text-sm font-medium text-gray-700">
          额度 (GB)
          <input type="number" min="1" step="1" required value={draft.budget_gb} onChange={(event) => setDraft((current) => ({ ...current, budget_gb: event.target.value }))} className="mt-1 h-10 w-full rounded-md border border-gray-300 px-3 text-sm focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500" />
        </label>
        <label className="text-sm font-medium text-gray-700">
          滚动周期 (小时)
          <input type="number" min="1" step="1" required value={draft.window_hours} onChange={(event) => setDraft((current) => ({ ...current, window_hours: event.target.value }))} className="mt-1 h-10 w-full rounded-md border border-gray-300 px-3 text-sm focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500" />
        </label>
        <button type="submit" disabled={creating || loading || configuredRemotes.length === 0} className="inline-flex h-10 items-center justify-center gap-2 rounded-md bg-blue-600 px-4 text-sm font-medium text-white hover:bg-blue-700 disabled:cursor-not-allowed disabled:opacity-50">
          <Plus className="h-4 w-4" />
          {creating ? '添加中...' : '添加账号'}
        </button>
      </form>

      {loading ? (
        <div className="flex h-40 items-center justify-center"><div className="h-8 w-8 animate-spin rounded-full border-b-2 border-blue-600" /></div>
      ) : accounts.length === 0 ? (
        <div className="flex min-h-40 flex-col items-center justify-center rounded-lg border border-dashed border-gray-300 bg-white px-4 text-center">
          <ShieldCheck className="h-8 w-8 text-gray-400" />
          <p className="mt-3 text-sm font-medium text-gray-700">暂无可信账号</p>
        </div>
      ) : (
        <div className="space-y-3">
          {accounts.map((account) => {
            const current = accountDraft(account);
            const saving = savingID === account.id;
            return (
              <section key={account.id} className={`rounded-lg border bg-white p-4 shadow-sm ${current.enabled ? 'border-gray-200' : 'border-gray-200 opacity-70'}`} aria-label={`${account.remote_name} 账号配置`}>
                <div className="grid gap-4 md:grid-cols-[minmax(0,1fr)_150px_150px_auto_auto] md:items-end">
                  <div className="min-w-0">
                    <div className="truncate text-sm font-semibold text-gray-900">{account.remote_name}</div>
                    <div className="mt-1 text-xs text-gray-500">账号 ID {account.id}</div>
                  </div>
                  <label className="text-sm font-medium text-gray-700">
                    额度 (GB)
                    <input type="number" min="1" step="1" value={current.budget_gb} onChange={(event) => updateDraft(account, { budget_gb: event.target.value })} className="mt-1 h-10 w-full rounded-md border border-gray-300 px-3 text-sm focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500" />
                  </label>
                  <label className="text-sm font-medium text-gray-700">
                    滚动周期 (小时)
                    <input type="number" min="1" step="1" value={current.window_hours} onChange={(event) => updateDraft(account, { window_hours: event.target.value })} className="mt-1 h-10 w-full rounded-md border border-gray-300 px-3 text-sm focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500" />
                  </label>
                  <label className="flex h-10 cursor-pointer items-center gap-2 text-sm font-medium text-gray-700">
                    <input type="checkbox" checked={current.enabled} onChange={(event) => updateDraft(account, { enabled: event.target.checked })} className="h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500" />
                    启用
                  </label>
                  <button type="button" onClick={() => saveAccount(account)} disabled={saving} className="inline-flex h-10 items-center justify-center gap-2 rounded-md border border-blue-200 px-3 text-sm font-medium text-blue-700 hover:bg-blue-50 disabled:opacity-50">
                    {saving ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}
                    保存
                  </button>
                </div>
              </section>
            );
          })}
        </div>
      )}
    </div>
  );
};

export default TrustedAccounts;
