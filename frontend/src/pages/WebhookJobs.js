import React, { useEffect, useMemo, useState } from 'react';
import {
  Bell,
  CheckCircle,
  Cog,
  Copy,
  Trash2,
  X,
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
import { createWebhookJob, deleteWebhookJob, getWebhookConfig, getWebhookJob, getWebhookJobs, retryWebhookJob, updateWebhookConfig } from '../services/api';

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
  tag_dirs: [],
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
  tag_dirs: Array.isArray(data?.tag_dirs) ? data.tag_dirs : [],
  allowed_callback_hosts: Array.isArray(data?.allowed_callback_hosts) ? data.allowed_callback_hosts.join(', ') : '',
  allowed_curl_hosts: Array.isArray(data?.allowed_curl_hosts) ? data.allowed_curl_hosts.join(', ') : '',
});

const WebhookJobs = ({ mode = 'all' }) => {
  const [jobs, setJobs] = useState([]);
  const [selectedJob, setSelectedJob] = useState(null);
  const [config, setConfig] = useState(null);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [savingConfig, setSavingConfig] = useState(false);
  const [detailOpen, setDetailOpen] = useState(false);
  const [form, setForm] = useState({
    path: '',
    tag: '',
    callback_url: '',
    curl_url: '',
    curl_headers: '',
  });
  const [configForm, setConfigForm] = useState(defaultWebhookConfig);

  const webhookEndpoint = useMemo(() => `${window.location.origin}/webhook`, []);
  const showConfig = mode === 'all' || mode === 'config';
  const showTasks = mode === 'all' || mode === 'tasks';
  const pageTitle = showConfig && !showTasks ? 'Webhook 配置' : 'Webhook 任务';
  const pageDescription = showConfig && !showTasks
    ? '配置 /webhook 接入参数、远端、Tag 保存目录、白名单、并发和超时。'
    : '查看外部 POST /webhook 创建的一次性下载任务，跟踪下载、校验、回调和刷新详情。';

  const loadData = async () => {
    setLoading(true);
    try {
      const [jobsRes, configRes] = await Promise.all([getWebhookJobs(), getWebhookConfig()]);
      const nextJobs = jobsRes.data.jobs || [];
      setJobs(nextJobs);
      if (selectedJob) {
        const latestSelected = nextJobs.find((job) => job.id === selectedJob.id);
        if (latestSelected) {
          setSelectedJob(latestSelected);
        } else {
          setSelectedJob(null);
          setDetailOpen(false);
        }
      }
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
        } else {
          setSelectedJob(null);
          setDetailOpen(false);
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
      setDetailOpen(true);
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
      setForm({ path: '', tag: '', callback_url: '', curl_url: '', curl_headers: '' });
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
        tag_dirs: configForm.tag_dirs,
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

  const deleteJob = async (job) => {
    if (!job) return;
    if (!['success', 'failed'].includes(job.status)) {
      toast.error('只能删除成功或失败的历史任务');
      return;
    }
    if (!window.confirm(`确定删除历史任务 ${job.id}？`)) {
      return;
    }
    try {
      await deleteWebhookJob(job.id);
      toast.success('历史任务已删除');
      setJobs((prev) => prev.filter((item) => item.id !== job.id));
      if (selectedJob?.id === job.id) {
        setSelectedJob(null);
        setDetailOpen(false);
      }
      await loadJobs();
    } catch (err) {
      toast.error(err.response?.data?.error || '删除失败');
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

  const updateTagDir = (index, field, value) => {
    setConfigForm((prev) => ({
      ...prev,
      tag_dirs: prev.tag_dirs.map((item, itemIndex) => (itemIndex === index ? { ...item, [field]: value } : item)),
    }));
  };

  const addTagDir = () => {
    setConfigForm((prev) => ({ ...prev, tag_dirs: [...prev.tag_dirs, { tag: '', dir: '' }] }));
  };

  const removeTagDir = (index) => {
    setConfigForm((prev) => ({ ...prev, tag_dirs: prev.tag_dirs.filter((_, itemIndex) => itemIndex !== index) }));
  };

  const closeDetail = () => {
    setDetailOpen(false);
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
          <h1 className="text-2xl font-bold text-gray-900">{pageTitle}</h1>
          <p className="text-gray-500 mt-1">{pageDescription}</p>
        </div>
        <button
          onClick={loadData}
          className="inline-flex items-center justify-center gap-2 px-4 py-2 bg-white border border-gray-200 rounded-lg hover:bg-gray-50 text-gray-700 font-medium"
        >
          <RefreshCw className="w-4 h-4" />
          刷新
        </button>
      </div>

      <div className={`grid grid-cols-1 gap-6 ${showConfig && showTasks ? 'xl:grid-cols-[420px,1fr]' : ''}`}>
        {(showConfig || showTasks) && (
        <div className="space-y-6 xl:max-h-[calc(100vh-7rem)] xl:overflow-y-auto xl:pr-2">
          {showConfig && (
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
                <p className="mt-1 text-xs text-gray-500">兼容旧配置；新 Webhook 任务按下方 Tag 保存目录落盘。</p>
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
              <div className="space-y-3">
                <div className="flex items-center justify-between gap-3">
                  <div>
                    <div className="text-sm font-medium text-gray-700">Tag 保存目录</div>
                    <p className="text-xs text-gray-500 mt-1">Webhook 传入 tag 后，会保存到对应目录下的最后一级文件夹。</p>
                  </div>
                  <button
                    type="button"
                    onClick={addTagDir}
                    className="px-3 py-1.5 text-sm rounded-lg bg-blue-50 text-blue-700 hover:bg-blue-100"
                  >
                    添加 Tag
                  </button>
                </div>
                {configForm.tag_dirs.length === 0 ? (
                  <div className="rounded-lg border border-dashed border-gray-200 px-3 py-4 text-sm text-gray-500">
                    暂未配置 tag。未配置的 tag 会被拒绝。
                  </div>
                ) : (
                  <div className="space-y-2">
                    {configForm.tag_dirs.map((item, index) => (
                      <div key={`${index}-${item.tag}`} className="grid grid-cols-1 md:grid-cols-[120px,1fr,auto] gap-2">
                        <input
                          value={item.tag}
                          onChange={(event) => updateTagDir(index, 'tag', event.target.value)}
                          placeholder="动画电影"
                          className="px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                        />
                        <input
                          value={item.dir}
                          onChange={(event) => updateTagDir(index, 'dir', event.target.value)}
                          placeholder="/opt/adjak"
                          className="px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                        />
                        <button
                          type="button"
                          onClick={() => removeTagDir(index)}
                          className="px-3 py-2 text-sm rounded-lg bg-gray-100 text-gray-700 hover:bg-gray-200"
                        >
                          删除
                        </button>
                      </div>
                    ))}
                  </div>
                )}
              </div>
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
              <div className="rounded-lg bg-blue-50 border border-blue-100 p-3 text-sm text-blue-800">
                Webhook 必须携带系统设置里的 API Token。远端名在这里配置，外部请求只需要传远端路径。
              </div>
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
          )}

          {showTasks && (
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
              <Field label="Tag">
                <input
                  value={form.tag}
                  onChange={(event) => setForm((prev) => ({ ...prev, tag: event.target.value }))}
                  placeholder="动画电影"
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
          )}

          {showConfig && (
          <div className="bg-slate-950 text-slate-100 rounded-xl shadow-sm border border-slate-800 p-6 overflow-hidden">
            <h2 className="text-lg font-semibold mb-4 flex items-center gap-2">
              <ShieldCheck className="w-5 h-5 text-emerald-300" />
              接入信息
            </h2>
            <Info label="Endpoint" value={webhookEndpoint} mono />
            <Info label="Remote" value={config?.rclone_remote || '未配置，请在本页 Webhook 运行配置中填写'} mono />
            <Info label="Tag Dirs" value={formatTagDirs(config?.tag_dirs)} mono />
            <Info label="Token" value={config?.api_token_enabled ? '已配置：Authorization: Bearer <token>' : '未配置，/webhook 会拒绝请求'} />
            <div className="mt-4 text-xs text-slate-400 leading-6">
              请求体必须包含 path、tag、callback_url。系统按 tag 映射目录保存，例如 tag=动画电影、目录=/opt/adjak、path=/up1/电影/绝命毒师，会保存到 /opt/adjak/绝命毒师。
            </div>
          </div>
          )}
        </div>
        )}

        {showTasks && (
        <div className="space-y-6 min-w-0">
          <div className="flex items-center justify-between gap-3">
            <div>
              <h2 className="text-lg font-semibold text-gray-900">Webhook 任务</h2>
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
                  onDelete={() => deleteJob(job)}
                  onCopy={() => copyID(job.id)}
                />
              ))}
            </div>
          )}

          {!selectedJob && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6 min-w-0 text-gray-500 text-sm">
              点击任务卡片中的“详情”以弹出卡片查看完整下载、通知和 rclone 日志。
            </div>
          )}
        </div>
        )}
      </div>

      {selectedJob && detailOpen && (
        <JobDetailModal
          job={selectedJob}
          onClose={closeDetail}
          onRetry={() => retryJob(selectedJob)}
          onDelete={() => deleteJob(selectedJob)}
        />
      )}
    </div>
  );
};

const JobCard = ({ job, active, onDetail, onRetry, onDelete, onCopy }) => {
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
          <CardLine icon={MapPin} label="远端" value={formatRemote(job)} mono />
          <CardLine icon={FileText} label="Tag" value={job.tag || '未设置'} />
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
          {['success', 'failed'].includes(job.status) && (
            <button onClick={onDelete} className="px-3 py-1.5 text-sm bg-gray-100 text-gray-700 rounded-lg hover:bg-red-50 hover:text-red-700 inline-flex items-center gap-1" type="button">
              <Trash2 className="w-3.5 h-3.5" /> 删除
            </button>
          )}
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

const JobDetailModal = ({ job, onClose, onRetry, onDelete }) => {
  const deletable = ['success', 'failed'].includes(job.status);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-950/60 px-3 py-6 backdrop-blur-sm" role="dialog" aria-modal="true" aria-labelledby="webhook-job-detail-title">
      <div className="w-full max-w-5xl max-h-[92vh] overflow-hidden rounded-3xl bg-white shadow-2xl border border-slate-200">
        <div className="flex flex-col md:flex-row md:items-start justify-between gap-4 border-b border-slate-100 bg-slate-50/80 px-5 md:px-6 py-4">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2 mb-2">
              <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full bg-slate-900 text-white text-xs font-semibold">
                <CheckCircle className="w-3.5 h-3.5" /> 一次性任务
              </span>
              <StatusBadge status={job.status} />
            </div>
            <h2 id="webhook-job-detail-title" className="text-lg font-semibold text-slate-950 break-all font-mono">
              {job.id}
            </h2>
            <p className="text-sm text-slate-500 mt-1">下载、校验、回调与 rclone 日志详情</p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {job.status === 'failed' && (
              <button onClick={onRetry} className="inline-flex items-center gap-1.5 px-3 py-2 text-sm rounded-xl bg-red-50 text-red-700 hover:bg-red-100" type="button">
                <RotateCcw className="w-4 h-4" /> 重试
              </button>
            )}
            {deletable && (
              <button onClick={onDelete} className="inline-flex items-center gap-1.5 px-3 py-2 text-sm rounded-xl bg-slate-100 text-slate-700 hover:bg-red-50 hover:text-red-700" type="button">
                <Trash2 className="w-4 h-4" /> 删除历史
              </button>
            )}
            <button onClick={onClose} className="inline-flex items-center justify-center w-10 h-10 rounded-xl bg-white text-slate-500 border border-slate-200 hover:bg-slate-100 hover:text-slate-900" type="button" aria-label="关闭任务详情">
              <X className="w-5 h-5" />
            </button>
          </div>
        </div>
        <div className="overflow-y-auto max-h-[calc(92vh-116px)] px-5 md:px-6 py-5">
          <JobDetail job={job} />
        </div>
      </div>
    </div>
  );
};

const JobDetail = ({ job }) => (
  <div className="space-y-5">
    <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
      <DetailItem label="Job ID" value={job.id} mono />
      <DetailItem label="任务类型" value={job.job_type || 'one_time'} />
      <DetailItem label="远端名" value={job.remote || '-'} mono />
      <DetailItem label="Tag" value={job.tag || '-'} />
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
    <div className={`mt-1 text-sm break-all whitespace-pre-wrap ${mono ? 'font-mono' : ''}`}>{value}</div>
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

const formatRemote = (job) => (job.remote ? `${job.remote}:${job.remote_path || ''}` : job.remote_path);

const formatTagDirs = (items) => {
  if (!Array.isArray(items) || items.length === 0) return '未配置';
  return items.map((item) => `${item.tag || '-'} => ${item.dir || '-'}`).join('\n');
};

const formatTime = (value) => {
  if (!value) return '-';
  return new Date(value).toLocaleString();
};

export default WebhookJobs;
