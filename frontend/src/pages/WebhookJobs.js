import React, { useEffect, useMemo, useState } from 'react';
import {
  Bell,
  Cog,
  Copy,
  ExternalLink,
  FileText,
  MapPin,
  RefreshCw,
  RotateCcw,
  Send,
  ShieldCheck,
  Terminal,
  Webhook,
} from 'lucide-react';
import toast from 'react-hot-toast';
import { createWebhookJob, getWebhookConfig, getWebhookJob, getWebhookJobs, retryWebhookJob, updateWebhookConfig } from '../services/api';

const statusStyles = {
  pending: 'bg-slate-100 text-slate-700 border-slate-200',
  running: 'bg-blue-100 text-blue-700 border-blue-200',
  copying: 'bg-indigo-100 text-indigo-700 border-indigo-200',
  checking: 'bg-purple-100 text-purple-700 border-purple-200',
  notifying_callback: 'bg-cyan-100 text-cyan-700 border-cyan-200',
  calling_curl_url: 'bg-cyan-100 text-cyan-700 border-cyan-200',
  success: 'bg-green-100 text-green-700 border-green-200',
  failed: 'bg-red-100 text-red-700 border-red-200',
};

const statusLabels = {
  pending: '等待中',
  running: '运行中',
  copying: '下载中',
  checking: '校验中',
  notifying_callback: '回调中',
  calling_curl_url: '刷新中',
  success: '成功',
  failed: '失败',
};

const defaultWebhookConfig = {
  local_base_dir: '/app/data/downloads',
  rclone_remote: '',
  transfers: 4,
  checkers: 8,
  retries: 3,
  low_level_retries: 10,
  bwlimit: '',
  job_timeout: '0s',
  http_timeout: '30s',
  max_rclone_log_bytes: 1048576,
  allow_anonymous_webhook: false,
  allowed_callback_hosts: '',
  allowed_curl_hosts: '',
};

const configToForm = (data) => ({
  local_base_dir: data?.local_base_dir || defaultWebhookConfig.local_base_dir,
  rclone_remote: data?.rclone_remote || '',
  transfers: data?.transfers ?? defaultWebhookConfig.transfers,
  checkers: data?.checkers ?? defaultWebhookConfig.checkers,
  retries: data?.retries ?? defaultWebhookConfig.retries,
  low_level_retries: data?.low_level_retries ?? defaultWebhookConfig.low_level_retries,
  bwlimit: data?.bwlimit || '',
  job_timeout: data?.job_timeout || defaultWebhookConfig.job_timeout,
  http_timeout: data?.http_timeout || defaultWebhookConfig.http_timeout,
  max_rclone_log_bytes: data?.max_rclone_log_bytes ?? defaultWebhookConfig.max_rclone_log_bytes,
  allow_anonymous_webhook: Boolean(data?.allow_anonymous_webhook),
  allowed_callback_hosts: Array.isArray(data?.allowed_callback_hosts) ? data.allowed_callback_hosts.join(', ') : '',
  allowed_curl_hosts: Array.isArray(data?.allowed_curl_hosts) ? data.allowed_curl_hosts.join(', ') : '',
});

const WebhookJobs = () => {
  const [jobs, setJobs] = useState([]);
  const [selectedJob, setSelectedJob] = useState(null);
  const [config, setConfig] = useState(null);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [savingConfig, setSavingConfig] = useState(false);
  const [form, setForm] = useState({
    path: '',
    callback_url: '',
    curl_url: '',
    curl_headers: '',
  });
  const [configForm, setConfigForm] = useState(defaultWebhookConfig);

  const webhookEndpoint = useMemo(() => `${window.location.origin}/webhook`, []);

  const loadData = async () => {
    setLoading(true);
    try {
      const [jobsRes, configRes] = await Promise.all([getWebhookJobs(), getWebhookConfig()]);
      setJobs(jobsRes.data.jobs || []);
      setConfig(configRes.data);
      setConfigForm(configToForm(configRes.data));
    } catch (err) {
      toast.error('加载 Webhook 下载数据失败');
    } finally {
      setLoading(false);
    }
  };

  const loadJobs = async () => {
    try {
      const res = await getWebhookJobs();
      const nextJobs = res.data.jobs || [];
      setJobs(nextJobs);
      if (selectedJob) {
        const latestSelected = nextJobs.find((job) => job.id === selectedJob.id);
        if (latestSelected) {
          setSelectedJob(latestSelected);
        }
      }
    } catch (err) {
      // keep silent during polling
    }
  };

  useEffect(() => {
    loadData();
    const interval = setInterval(() => {
      loadJobs();
    }, 5000);
    return () => clearInterval(interval);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const loadJob = async (id) => {
    try {
      const res = await getWebhookJob(id);
      setSelectedJob(res.data);
    } catch (err) {
      toast.error('查询任务失败');
    }
  };

  const submitJob = async (event) => {
    event.preventDefault();
    setSubmitting(true);
    try {
      const payload = { ...form };
      if (form.curl_headers.trim()) {
        payload.curl_headers = JSON.parse(form.curl_headers);
      } else {
        delete payload.curl_headers;
      }
      const res = await createWebhookJob(payload);
      toast.success(`一次性任务已创建：${res.data.job_id}`);
      setForm({ path: '', callback_url: '', curl_url: '', curl_headers: '' });
      await loadJobs();
      await loadJob(res.data.job_id);
    } catch (err) {
      toast.error(err.response?.data?.error || err.message || '创建失败');
    } finally {
      setSubmitting(false);
    }
  };

  const saveWebhookConfig = async (event) => {
    event.preventDefault();
    setSavingConfig(true);
    try {
      const payload = {
        ...configForm,
        transfers: Number(configForm.transfers),
        checkers: Number(configForm.checkers),
        retries: Number(configForm.retries),
        low_level_retries: Number(configForm.low_level_retries),
        max_rclone_log_bytes: Number(configForm.max_rclone_log_bytes),
        allowed_callback_hosts: splitHosts(configForm.allowed_callback_hosts),
        allowed_curl_hosts: splitHosts(configForm.allowed_curl_hosts),
      };
      await updateWebhookConfig(payload);
      const res = await getWebhookConfig();
      setConfig(res.data);
      setConfigForm(configToForm(res.data));
      toast.success('Webhook 配置已保存');
    } catch (err) {
      toast.error(err.response?.data?.error || '保存 Webhook 配置失败');
    } finally {
      setSavingConfig(false);
    }
  };

  const retryJob = async (job) => {
    try {
      await retryWebhookJob(job.id);
      toast.success('已重新入队');
      await loadJobs();
      await loadJob(job.id);
    } catch (err) {
      toast.error(err.response?.data?.error || '重试失败');
    }
  };

  const copyID = async (jobID) => {
    try {
      await navigator.clipboard.writeText(jobID);
      toast.success('Job ID 已复制');
    } catch (err) {
      toast.error('复制失败');
    }
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600" />
      </div>
    );
  }

  return (
    <div className="space-y-6 min-w-0">
      <div className="flex flex-col lg:flex-row lg:items-end justify-between gap-4">
        <div>
          <div className="inline-flex items-center gap-2 px-3 py-1 rounded-full bg-blue-50 text-blue-700 text-sm font-medium mb-3">
            <Webhook className="w-4 h-4" />
            Webhook One-Time Jobs
          </div>
          <h1 className="text-2xl font-bold text-gray-900">Webhook 一次性下载</h1>
          <p className="text-gray-500 mt-1">外部 POST /webhook 后会立刻出现在这里。每条记录都是一次性任务，可查看下载、校验、回调和刷新详情。</p>
        </div>
        <button
          onClick={loadData}
          className="inline-flex items-center justify-center gap-2 px-4 py-2 bg-white border border-gray-200 rounded-lg hover:bg-gray-50 text-gray-700 font-medium"
        >
          <RefreshCw className="w-4 h-4" />
          刷新
        </button>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-[420px,1fr] gap-6">
        <div className="space-y-6 xl:max-h-[calc(100vh-7rem)] xl:overflow-y-auto xl:pr-2">
          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
            <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
              <Cog className="w-5 h-5 text-blue-500" />
              Webhook 运行配置
            </h2>
            <form onSubmit={saveWebhookConfig} className="space-y-4">
              <Field label="Rclone 远端名">
                <input
                  value={configForm.rclone_remote}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, rclone_remote: event.target.value }))}
                  placeholder="webdav"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="本地下载根目录">
                <input
                  value={configForm.local_base_dir}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, local_base_dir: event.target.value }))}
                  placeholder="/app/data/downloads"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                />
              </Field>
              <div className="grid grid-cols-2 gap-3">
                <Field label="传输并发数">
                  <input
                    type="number"
                    min="1"
                    max="64"
                    value={configForm.transfers}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, transfers: event.target.value }))}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
                <Field label="校验并发数">
                  <input
                    type="number"
                    min="1"
                    max="128"
                    value={configForm.checkers}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, checkers: event.target.value }))}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
                <Field label="失败重试次数">
                  <input
                    type="number"
                    min="0"
                    max="20"
                    value={configForm.retries}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, retries: event.target.value }))}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
                <Field label="底层重试次数">
                  <input
                    type="number"
                    min="0"
                    max="50"
                    value={configForm.low_level_retries}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, low_level_retries: event.target.value }))}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
              </div>
              <Field label="传输限速（可选）">
                <input
                  value={configForm.bwlimit}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, bwlimit: event.target.value }))}
                  placeholder="10M 或留空"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <div className="grid grid-cols-2 gap-3">
                <Field label="任务超时">
                  <input
                    value={configForm.job_timeout}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, job_timeout: event.target.value }))}
                    placeholder="0s"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
                <Field label="回调请求超时">
                  <input
                    value={configForm.http_timeout}
                    onChange={(event) => setConfigForm((prev) => ({ ...prev, http_timeout: event.target.value }))}
                    placeholder="30s"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                </Field>
              </div>
              <Field label="Rclone 日志最大字节数">
                <input
                  type="number"
                  min="1024"
                  max="10485760"
                  value={configForm.max_rclone_log_bytes}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, max_rclone_log_bytes: event.target.value }))}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Callback host 白名单（逗号分隔，留空允许全部）">
                <input
                  value={configForm.allowed_callback_hosts}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, allowed_callback_hosts: event.target.value }))}
                  placeholder="sender.example.com, *.example.com"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Curl host 白名单（逗号分隔，留空允许全部）">
                <input
                  value={configForm.allowed_curl_hosts}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, allowed_curl_hosts: event.target.value }))}
                  placeholder="localhost, 127.0.0.1, api.example.com"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <label className="flex items-start gap-3 rounded-lg bg-amber-50 border border-amber-100 p-3 text-sm text-amber-800">
                <input
                  type="checkbox"
                  checked={configForm.allow_anonymous_webhook}
                  onChange={(event) => setConfigForm((prev) => ({ ...prev, allow_anonymous_webhook: event.target.checked }))}
                  className="mt-1 rounded border-amber-300 text-blue-600 focus:ring-blue-500"
                />
                <span>允许 /webhook 匿名调用。仅内网可信环境建议开启。</span>
              </label>
              <button
                type="submit"
                disabled={savingConfig}
                className="w-full inline-flex items-center justify-center gap-2 px-4 py-2 bg-gray-900 text-white rounded-lg hover:bg-gray-800 font-medium disabled:opacity-50"
              >
                <Cog className="w-4 h-4" />
                {savingConfig ? '保存中...' : '保存 Webhook 配置'}
              </button>
            </form>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
            <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
              <Send className="w-5 h-5 text-blue-500" />
              手动创建一次性任务
            </h2>
            <form onSubmit={submitJob} className="space-y-4">
              <Field label="远端路径">
                <input
                  value={form.path}
                  onChange={(event) => setForm((prev) => ({ ...prev, path: event.target.value }))}
                  placeholder="/remote/folder/a"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Callback URL">
                <input
                  value={form.callback_url}
                  onChange={(event) => setForm((prev) => ({ ...prev, callback_url: event.target.value }))}
                  placeholder="https://sender.example.com/download-finished"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Curl URL">
                <input
                  value={form.curl_url}
                  onChange={(event) => setForm((prev) => ({ ...prev, curl_url: event.target.value }))}
                  placeholder="http://localhost:5244/api/fs/list?path=/&refresh=true"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Curl Headers（JSON，可选）">
                <textarea
                  value={form.curl_headers}
                  onChange={(event) => setForm((prev) => ({ ...prev, curl_headers: event.target.value }))}
                  placeholder={'{\n  "Authorization": "openlist-xxx"\n}'}
                  rows={4}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-xs"
                />
              </Field>
              <button
                type="submit"
                disabled={submitting}
                className="w-full inline-flex items-center justify-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 font-medium disabled:opacity-50"
              >
                <Send className="w-4 h-4" />
                {submitting ? '提交中...' : '提交一次性任务'}
              </button>
            </form>
          </div>

          <div className="bg-slate-950 text-slate-100 rounded-xl shadow-sm border border-slate-800 p-6 overflow-hidden">
            <h2 className="text-lg font-semibold mb-4 flex items-center gap-2">
              <ShieldCheck className="w-5 h-5 text-emerald-300" />
              接入信息
            </h2>
            <Info label="Endpoint" value={webhookEndpoint} mono />
            <Info label="Remote" value={config?.rclone_remote || '未配置，请在本页 Webhook 运行配置中填写'} mono />
            <Info label="Local Base" value={config?.local_base_dir || '-'} mono />
            <Info label="Token" value={config?.token_required ? '使用系统设置中的 API Token：Authorization: Bearer <token>' : 'Webhook 未要求 Token'} />
            <div className="mt-4 text-xs text-slate-400 leading-6">
              Webhook 与输出日志 API 共用同一个访问 Token。通过 curl 发出的任务会写入 SQLite，页面每 5 秒自动刷新。callback/curl host 白名单为空时允许所有 HTTP(S) 主机。
            </div>
          </div>
        </div>

        <div className="space-y-6 min-w-0">
          <div className="flex items-center justify-between gap-3">
            <div>
              <h2 className="text-lg font-semibold text-gray-900">一次性任务卡片</h2>
              <p className="text-sm text-gray-500 mt-1">共 {jobs.length} 条，最新任务在前。</p>
            </div>
          </div>

          {jobs.length === 0 ? (
            <div className="bg-white rounded-xl shadow-sm border border-gray-200 px-6 py-12 text-center text-gray-500">
              暂无一次性任务。外部调用 <span className="font-mono text-gray-700">POST /webhook</span> 后会显示在这里。
            </div>
          ) : (
            <div className="grid grid-cols-1 2xl:grid-cols-2 gap-4">
              {jobs.map((job) => (
                <JobCard
                  key={job.id}
                  job={job}
                  active={selectedJob?.id === job.id}
                  onDetail={() => loadJob(job.id)}
                  onRetry={() => retryJob(job)}
                  onCopy={() => copyID(job.id)}
                />
              ))}
            </div>
          )}

          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6 min-w-0">
            <div className="flex items-center justify-between gap-3 mb-4">
              <h2 className="text-lg font-semibold text-gray-900">任务详情</h2>
              {selectedJob && <StatusBadge status={selectedJob.status} />}
            </div>
            {selectedJob ? (
              <JobDetail job={selectedJob} />
            ) : (
              <div className="text-gray-500 text-sm">点击任务卡片中的“详情”查看完整下载、通知和 rclone 日志。</div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

const JobCard = ({ job, active, onDetail, onRetry, onCopy }) => {
  const headerCount = parseCurlHeaders(job.curl_headers).length;

  return (
    <article className={`rounded-2xl border bg-white shadow-sm transition-all ${active ? 'border-blue-300 ring-2 ring-blue-100' : 'border-gray-200 hover:border-blue-200'}`}>
      <div className="p-5 space-y-4">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2 mb-2">
              <span className="px-2 py-1 rounded-full text-xs font-semibold bg-gray-900 text-white">一次性任务</span>
              <StatusBadge status={job.status} />
            </div>
            <button onClick={onDetail} className="font-mono text-xs text-blue-700 hover:text-blue-900 break-all text-left">
              {job.id}
            </button>
          </div>
          <button onClick={onCopy} className="p-2 text-gray-400 hover:text-gray-700 hover:bg-gray-100 rounded-lg" title="复制 Job ID" type="button">
            <Copy className="w-4 h-4" />
          </button>
        </div>

        <div className="space-y-2 text-sm">
          <CardLine icon={MapPin} label="远端" value={job.remote_path} mono />
          <CardLine icon={FileText} label="本地" value={job.local_path || '等待生成'} mono muted={!job.local_path} />
          <CardLine icon={Bell} label="Callback" value={hostOf(job.callback_url)} />
          <CardLine icon={ExternalLink} label="Curl" value={hostOf(job.curl_url)} />
        </div>

        <div className="grid grid-cols-2 gap-2 text-xs">
          <MiniMetric label="Header" value={`${headerCount} 个`} />
          <MiniMetric label="更新" value={formatTime(job.updated_at)} />
        </div>

        {job.error && (
          <div className="rounded-lg bg-red-50 border border-red-100 px-3 py-2 text-sm text-red-700 line-clamp-2">
            {job.error}
          </div>
        )}

        <div className="flex items-center justify-end gap-2 pt-1">
          <button onClick={onDetail} className="px-3 py-1.5 text-sm bg-gray-100 text-gray-700 rounded-lg hover:bg-gray-200" type="button">
            详情
          </button>
          {job.status === 'failed' && (
            <button onClick={onRetry} className="px-3 py-1.5 text-sm bg-red-50 text-red-700 rounded-lg hover:bg-red-100 inline-flex items-center gap-1" type="button">
              <RotateCcw className="w-3.5 h-3.5" /> 重试
            </button>
          )}
        </div>
      </div>
    </article>
  );
};

const JobDetail = ({ job }) => (
  <div className="space-y-5">
    <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
      <DetailItem label="Job ID" value={job.id} mono />
      <DetailItem label="任务类型" value={job.job_type || 'one_time'} />
      <DetailItem label="远端路径" value={job.remote_path} mono />
      <DetailItem label="本地路径" value={job.local_path || '-'} mono />
      <DetailItem label="创建时间" value={formatTime(job.created_at)} />
      <DetailItem label="更新时间" value={formatTime(job.updated_at)} />
      <DetailItem label="完成时间" value={formatTime(job.finished_at)} />
      <DetailItem label="Curl Headers" value={formatHeaders(job.curl_headers)} mono />
    </div>

    <DetailBlock label="Callback URL" value={job.callback_url} />
    <DetailBlock label="Curl URL" value={job.curl_url} />
    {job.error && <DetailBlock label="错误" value={job.error} danger />}

    <div>
      <div className="flex items-center gap-2 text-sm font-semibold text-gray-900 mb-2">
        <Terminal className="w-4 h-4 text-gray-500" />
        Rclone 日志
      </div>
      <pre className="bg-gray-950 text-gray-100 rounded-lg p-4 text-xs overflow-auto max-h-[460px] whitespace-pre-wrap">
        {job.rclone_log || '暂无日志'}
      </pre>
    </div>
  </div>
);

const Field = ({ label, children }) => (
  <label className="block">
    <span className="block text-sm font-medium text-gray-700 mb-1">{label}</span>
    {children}
  </label>
);

const Info = ({ label, value, mono }) => (
  <div className="py-2 border-b border-slate-800 last:border-b-0">
    <div className="text-xs uppercase tracking-wide text-slate-500">{label}</div>
    <div className={`mt-1 text-sm break-all ${mono ? 'font-mono' : ''}`}>{value}</div>
  </div>
);

const CardLine = ({ icon: Icon, label, value, mono, muted }) => (
  <div className="flex items-start gap-2 min-w-0">
    <Icon className="w-4 h-4 text-gray-400 mt-0.5 flex-shrink-0" />
    <div className="min-w-0">
      <div className="text-xs text-gray-500">{label}</div>
      <div className={`truncate ${mono ? 'font-mono text-xs' : ''} ${muted ? 'text-gray-400' : 'text-gray-800'}`}>{value || '-'}</div>
    </div>
  </div>
);

const MiniMetric = ({ label, value }) => (
  <div className="rounded-lg bg-gray-50 border border-gray-100 px-3 py-2">
    <div className="text-gray-400">{label}</div>
    <div className="font-medium text-gray-700 truncate">{value}</div>
  </div>
);

const DetailItem = ({ label, value, mono }) => (
  <div className="rounded-lg bg-gray-50 border border-gray-100 px-3 py-2 min-w-0">
    <div className="text-xs text-gray-500 mb-1">{label}</div>
    <div className={`text-sm text-gray-800 break-all ${mono ? 'font-mono text-xs' : ''}`}>{value || '-'}</div>
  </div>
);

const DetailBlock = ({ label, value, danger }) => (
  <div>
    <div className="text-sm font-semibold text-gray-900 mb-2">{label}</div>
    <div className={`rounded-lg border px-3 py-2 text-sm break-all ${danger ? 'bg-red-50 border-red-100 text-red-700' : 'bg-gray-50 border-gray-100 text-gray-800'}`}>
      {value || '-'}
    </div>
  </div>
);

const StatusBadge = ({ status }) => (
  <span className={`inline-flex items-center px-2 py-1 rounded-full text-xs font-medium border ${statusStyles[status] || 'bg-gray-100 text-gray-700 border-gray-200'}`}>
    {statusLabels[status] || status}
  </span>
);

const parseCurlHeaders = (value) => {
  if (!value) return [];
  try {
    return Object.entries(JSON.parse(value));
  } catch (err) {
    return [];
  }
};

const splitHosts = (value) => value
  .split(',')
  .map((item) => item.trim())
  .filter(Boolean);

const formatHeaders = (value) => {
  const headers = parseCurlHeaders(value);
  if (headers.length === 0) return '-';
  return headers.map(([key, val]) => `${key}: ${maskHeaderValue(key, val)}`).join('\n');
};

const maskHeaderValue = (key, value) => {
  if (!value) return '';
  if (key.toLowerCase() === 'authorization') {
    return `${String(value).slice(0, 12)}...${String(value).slice(-6)}`;
  }
  return value;
};

const hostOf = (value) => {
  if (!value) return '-';
  try {
    return new URL(value).host;
  } catch (err) {
    return value;
  }
};

const formatTime = (value) => {
  if (!value) return '-';
  return new Date(value).toLocaleString();
};

export default WebhookJobs;
